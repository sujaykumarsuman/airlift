// Command airlift-tower hosts an airlift session on the operator's laptop:
// it receives relayed QR frames from scanners over HTTPS on the LAN,
// reassembles and verifies the payload, unpacks repobundles, serves the
// dashboard and downloads, and writes verified results to --dest.
//
// With --replay it instead feeds a frames dump (or any file) into a fresh
// session over a private loopback listener, as if a phone were relaying,
// and exits 0 once the session is READY. See docs/BUILD-PLAN.md Phase 2.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	airlift "github.com/sujaykumarsuman/airlift"
	"github.com/sujaykumarsuman/airlift/internal/replay"
	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/tlsca"
)

const (
	maxSessions  = 32
	leafValidity = 7 * 24 * time.Hour
	sweepEvery   = 30 * time.Second
)

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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	var c config
	flags := flag.NewFlagSet("airlift-tower", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&c.dest, "dest", "", "directory that receives every verified result (raw file and unpacked tree)")
	flags.StringVar(&c.bind, "bind", "", "LAN IPv4 to bind (default: the first detected LAN address)")
	flags.IntVar(&c.port, "port", 8443, "HTTPS port")
	flags.StringVar(&c.certFile, "cert", "", "TLS certificate PEM; with --key, replaces the built-in CA (mkcert users)")
	flags.StringVar(&c.keyFile, "key", "", "TLS private key PEM")
	flags.DurationVar(&c.ttl, "ttl", time.Hour, "session time-to-live, refreshed on activity")
	flags.StringVar(&c.caDir, "ca-dir", "", "where the built-in CA lives (default: <user config dir>/airlift)")
	flags.BoolVar(&c.session, "session", false, "create a session at start and print its join QR (headless use)")
	flags.StringVar(&c.replayFile, "replay", "", "feed this frames dump (or any file) into a fresh session over loopback, then exit")
	flags.StringVar(&c.into, "into", "", "replay: feed a session on a running tower instead; its join URL, https://host:port/s/SID#t=TOKEN (dev use: the token is visible to ps)")
	flags.Float64Var(&c.rate, "rate", 8, "replay: decoded frames per second (0 = unpaced)")
	flags.Float64Var(&c.drop, "drop", 0.2, "replay: probability that each frame is missed")
	flags.BoolVar(&c.shuffle, "shuffle", false, "replay: reorder frames within each pass")
	flags.IntVar(&c.passes, "passes", 10, "replay: maximum loop passes")
	flags.Int64Var(&c.seed, "seed", 1, "replay: PRNG seed for drop and shuffle")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "airlift-tower: unexpected argument %q\n", flags.Arg(0))
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
	if c.replayFile != "" && c.into != "" {
		return runReplayInto(ctx, c, stdout, logger)
	}
	if c.replayFile != "" {
		return runReplay(ctx, c, stdout, logger)
	}
	if c.into != "" {
		logger.Printf("error: --into needs --replay FILE")
		return 2
	}
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
	fmt.Fprintf(stdout, "airlift-tower\n  dashboard  %s/\n", base)
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

func runReplay(ctx context.Context, c config, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	dump, encoded, err := replay.Load(c.replayFile)
	if err != nil {
		return fail(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	base := "http://" + ln.Addr().String()
	store := session.NewStore(c.ttl, maxSessions)
	srv := server.New(server.Options{Store: store, PublicBase: base, Dest: c.dest, Logf: logger.Printf})
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()

	s, _, err := srv.CreateSession()
	if err != nil {
		return fail(err)
	}
	m := dump.Manifest
	note := ""
	if encoded {
		note = " (not a frames dump: encoded on the fly)"
	}
	fmt.Fprintf(stdout, "replay %s%s\n", c.replayFile, note)
	fmt.Fprintf(stdout, "  %d chunks × %d bytes, %d → %d bytes gzip, sender session 0x%08x\n",
		m.Total(), m.Chunk, m.OrigSize, m.GzSize, dump.SenderSession)
	fmt.Fprintf(stdout, "  rate %g fps, drop %.0f%%, shuffle %v, passes ≤ %d, seed %d\n",
		c.rate, c.drop*100, c.shuffle, c.passes, c.seed)
	rep, err := replay.Run(ctx, &http.Client{Timeout: 30 * time.Second}, base, s.ID, s.Token, dump, replay.Options{
		Rate:    c.rate,
		Drop:    c.drop,
		Shuffle: c.shuffle,
		Passes:  c.passes,
		Seed:    c.seed,
		Logf:    func(format string, args ...any) { fmt.Fprintf(stdout, "  "+format+"\n", args...) },
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "  posted %d frames in %d pass(es): accepted %d, dup %d, bad %d\n",
		rep.Posted, rep.Passes, rep.Accepted, rep.Dup, rep.Bad)
	var snap session.Snapshot
	if err := json.Unmarshal(rep.Snapshot, &snap); err != nil {
		return fail(err)
	}
	printOutcome(stdout, snap)
	if snap.State == session.StateReady {
		return 0
	}
	return 1
}

// runReplayInto relays a dump into a session on a running tower, which is
// how the dashboard is exercised without a camera.
func runReplayInto(ctx context.Context, c config, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	dump, encoded, err := replay.Load(c.replayFile)
	if err != nil {
		return fail(err)
	}
	base, sid, token, err := parseJoinURL(c.into)
	if err != nil {
		return fail(err)
	}
	client, err := towerClient(c.caDir)
	if err != nil {
		return fail(err)
	}
	m := dump.Manifest
	note := ""
	if encoded {
		note = " (not a frames dump: encoded on the fly)"
	}
	fmt.Fprintf(stdout, "replay %s%s → %s session %s\n", c.replayFile, note, base, sid)
	fmt.Fprintf(stdout, "  %d chunks × %d bytes, %d → %d bytes gzip, sender session 0x%08x\n",
		m.Total(), m.Chunk, m.OrigSize, m.GzSize, dump.SenderSession)
	fmt.Fprintf(stdout, "  rate %g fps, drop %.0f%%, shuffle %v, passes ≤ %d, seed %d\n",
		c.rate, c.drop*100, c.shuffle, c.passes, c.seed)
	rep, err := replay.Run(ctx, client, base, sid, token, dump, replay.Options{
		Rate:    c.rate,
		Drop:    c.drop,
		Shuffle: c.shuffle,
		Passes:  c.passes,
		Seed:    c.seed,
		Logf:    func(format string, args ...any) { fmt.Fprintf(stdout, "  "+format+"\n", args...) },
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "  posted %d frames in %d pass(es): accepted %d, dup %d, bad %d\n",
		rep.Posted, rep.Passes, rep.Accepted, rep.Dup, rep.Bad)
	var snap session.Snapshot
	if err := json.Unmarshal(rep.Snapshot, &snap); err != nil {
		return fail(err)
	}
	printOutcome(stdout, snap)
	if snap.State == session.StateReady {
		return 0
	}
	return 1
}

// parseJoinURL splits https://host:port/s/SID#t=TOKEN into its parts.
func parseJoinURL(s string) (base, sid, token string, err error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", "", fmt.Errorf("--into: %w", err)
	}
	rest, ok := strings.CutPrefix(u.Path, "/s/")
	sid = strings.Trim(rest, "/")
	token = u.Query().Get("t")
	if frag, err := url.ParseQuery(u.Fragment); err == nil && frag.Get("t") != "" {
		token = frag.Get("t")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !ok || sid == "" || strings.Contains(sid, "/") || token == "" {
		return "", "", "", errors.New("--into: expected a join URL like https://host:8443/s/SID#t=TOKEN")
	}
	return u.Scheme + "://" + u.Host, sid, token, nil
}

// towerClient trusts the system roots plus the local CA in caDir (or the
// default config dir), so --into works against the built-in TLS.
func towerClient(caDir string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	dir := caDir
	if dir == "" {
		if base, err := os.UserConfigDir(); err == nil {
			dir = filepath.Join(base, "airlift")
		}
	}
	if dir != "" {
		if pemBytes, err := os.ReadFile(filepath.Join(dir, tlsca.CertFile)); err == nil {
			pool.AppendCertsFromPEM(pemBytes)
		}
	}
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("unexpected default transport")
	}
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}, nil
}

// quietTLS drops net/http's "TLS handshake error" lines: every phone's first
// visit before it installs the CA produces one, and the README covers that.
type quietTLS struct{ w io.Writer }

func (q quietTLS) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return q.w.Write(p)
}

func printOutcome(w io.Writer, snap session.Snapshot) {
	fmt.Fprintf(w, "state %s\n", snap.State)
	verdict := func(label string, v *session.Verdict) {
		if v == nil {
			return
		}
		mark := "OK "
		if !v.OK {
			mark = "BAD"
		}
		fmt.Fprintf(w, "  %s %-8s expected %s\n               actual   %s\n", mark, label, v.Expected, v.Actual)
	}
	verdict("gz_sha", snap.Verdicts.GzSHA)
	verdict("orig_sha", snap.Verdicts.OrigSHA)
	verdict("bundle", snap.Verdicts.Bundle)
	if snap.Bundle != nil {
		fmt.Fprintf(w, "  bundle    %d files, %d bytes\n", snap.Bundle.Files, snap.Bundle.TotalBytes)
	}
	if len(snap.Downloads) > 0 {
		fmt.Fprintf(w, "  downloads %s\n", strings.Join(snap.Downloads, ", "))
	}
	if snap.DestPath != nil {
		fmt.Fprintf(w, "  dest      %s\n", *snap.DestPath)
	}
	if snap.Error != nil {
		fmt.Fprintf(w, "  error     %s\n", *snap.Error)
	}
}

// printJoin shows the join link and its QR. The token is part of the link
// by design; it goes to the operator's terminal, never to the log.
func printJoin(w io.Writer, sid, join string) {
	fmt.Fprintf(w, "\nsession %s\njoin    %s\n\n", sid, join)
	code, err := terminalQR(join)
	if err != nil {
		fmt.Fprintf(w, "(no QR: %v)\n", err)
		return
	}
	io.WriteString(w, code)
}
