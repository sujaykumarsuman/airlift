package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	airlift "github.com/sujaykumarsuman/airlift"
	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/tlsca"
)

const (
	maxSessions  = 32
	leafValidity = 7 * 24 * time.Hour
	sweepEvery   = 30 * time.Second
)

// config holds the flags for the tower and replay subcommands; each fills the
// subset it uses.
type config struct {
	dest, bind        string
	port              int
	certFile, keyFile string
	ttl               time.Duration
	caDir             string
	session           bool
	replayFile, into  string
	rate, drop        float64
	shuffle           bool
	passes            int
	seed              int64
}

func cmdTower(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tower", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift tower [--dest DIR] [--bind IP] [--port N] [--cert FILE --key FILE] [--ttl D] [--session]\n\n")
		fs.PrintDefaults()
	}
	var c config
	fs.StringVar(&c.dest, "dest", "", "directory that receives every verified result (raw file and unpacked tree)")
	fs.StringVar(&c.bind, "bind", "", "LAN IPv4 to bind (default: the first detected LAN address)")
	fs.IntVar(&c.port, "port", 8443, "HTTPS port")
	fs.StringVar(&c.certFile, "cert", "", "TLS certificate PEM; with --key, replaces the built-in CA (mkcert users)")
	fs.StringVar(&c.keyFile, "key", "", "TLS private key PEM")
	fs.DurationVar(&c.ttl, "ttl", time.Hour, "session time-to-live, refreshed on activity")
	fs.StringVar(&c.caDir, "ca-dir", "", "where the built-in CA lives (default: <user config dir>/airlift)")
	fs.BoolVar(&c.session, "session", false, "create a session at start and print its join QR (headless use)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "airlift tower: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	logger := log.New(stderr, "", log.Ltime)
	if c.dest != "" {
		abs, err := filepath.Abs(c.dest)
		if err != nil {
			logger.Printf("error: --dest: %v", err)
			return 1
		}
		c.dest = abs
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, c, stdout, logger)
}

func runServe(ctx context.Context, c config, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	if (c.certFile == "") != (c.keyFile == "") {
		return fail(errors.New("--cert and --key must be given together"))
	}
	lan, lanErr := tlsca.LANIPv4s()
	var bindIP net.IP
	if c.bind != "" {
		if bindIP = net.ParseIP(c.bind); bindIP == nil {
			return fail(fmt.Errorf("--bind %q is not an IP address", c.bind))
		}
	} else {
		if lanErr != nil {
			return fail(lanErr)
		}
		bindIP = lan[0]
	}

	var cert tls.Certificate
	var caPEM []byte
	if c.certFile != "" {
		var err error
		if cert, err = tlsca.LoadPair(c.certFile, c.keyFile); err != nil {
			return fail(err)
		}
		logger.Printf("tls: using %s", c.certFile)
	} else {
		dir := c.caDir
		if dir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				return fail(fmt.Errorf("user config dir: %w (set --ca-dir)", err))
			}
			dir = filepath.Join(base, "airlift")
		}
		ca, created, err := tlsca.LoadOrCreate(dir)
		if err != nil {
			return fail(err)
		}
		if created {
			logger.Printf("tls: created local CA in %s", dir)
		} else {
			logger.Printf("tls: using local CA from %s", dir)
		}
		sans := append([]net.IP{bindIP}, lan...)
		if cert, err = ca.Leaf(sans, nil, leafValidity); err != nil {
			return fail(err)
		}
		caPEM = ca.CertPEM
	}

	store := session.NewStore(c.ttl, maxSessions)
	go store.Run(ctx, sweepEvery)
	base := fmt.Sprintf("https://%s:%d", bindIP, c.port)
	web, err := fs.Sub(airlift.Dist, "web/dist")
	if err != nil {
		return fail(err)
	}
	srv := server.New(server.Options{
		Store:      store,
		PublicBase: base,
		CACertPEM:  caPEM,
		Web:        web,
		Dest:       c.dest,
		OnCreate:   func(s *session.Session, join string) { printJoin(stdout, s.ID, join) },
		Logf:       logger.Printf,
	})
	ln, err := net.Listen("tcp", net.JoinHostPort(bindIP.String(), strconv.Itoa(c.port)))
	if err != nil {
		return fail(err)
	}
	hs := &http.Server{
		Handler:           srv.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          log.New(quietTLS{logger.Writer()}, "", log.Ltime),
	}
	fmt.Fprintf(stdout, "airlift tower\n  dashboard  %s/\n", base)
	if caPEM != nil {
		fmt.Fprintf(stdout, "  ca cert    %s/ca.crt   (install once per phone)\n", base)
	}
	if c.dest != "" {
		fmt.Fprintf(stdout, "  dest       %s\n", c.dest)
	}
	fmt.Fprintf(stdout, "  ttl        %s\n", c.ttl)
	if c.session {
		if _, _, err := srv.CreateSession(); err != nil {
			return fail(err)
		}
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			hs.Close()
		}
	}()
	if err := hs.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return fail(err)
	}
	logger.Printf("stopped")
	return 0
}
