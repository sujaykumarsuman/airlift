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
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/replay"
	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
	"github.com/sujaykumarsuman/airlift/internal/tlsca"
)

func cmdReplay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift replay FILE [--into JOIN_URL] [--dest DIR] [--rate F] [--drop F] [--shuffle] [--passes N] [--seed N]\n\n")
		fs.PrintDefaults()
	}
	var c config
	fs.StringVar(&c.into, "into", "", "feed a session on a running tower instead; its join URL, https://host:port/s/SID#t=TOKEN (dev use: the token is visible to ps)")
	fs.StringVar(&c.dest, "dest", "", "directory that receives the verified result (loopback tower only)")
	fs.Float64Var(&c.rate, "rate", 8, "decoded frames per second (0 = unpaced)")
	fs.Float64Var(&c.drop, "drop", 0.2, "probability that each frame is missed")
	fs.BoolVar(&c.shuffle, "shuffle", false, "reorder frames within each pass")
	fs.IntVar(&c.passes, "passes", 10, "maximum loop passes")
	fs.Int64Var(&c.seed, "seed", 1, "PRNG seed for drop and shuffle")
	fs.StringVar(&c.caDir, "ca-dir", "", "where the built-in CA lives, for --into over the tower's TLS")
	positional, err := parsePermuted(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintln(stderr, "airlift replay: expected exactly one FILE argument")
		return 2
	}
	c.replayFile = positional[0]
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
	if c.into != "" {
		return runReplayInto(ctx, c, stdout, logger)
	}
	return runReplay(ctx, c, stdout, logger)
}

func runReplay(ctx context.Context, c config, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	dump, encoded, err := beam.Load(c.replayFile)
	if err != nil {
		return fail(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	base := "http://" + ln.Addr().String()
	store := session.NewStore(time.Hour, maxSessions)
	srv := server.New(server.Options{Store: store, PublicBase: base, Dest: c.dest, Logf: logger.Printf})
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	defer hs.Close()

	s, _, err := srv.CreateSession()
	if err != nil {
		return fail(err)
	}
	printReplayHeader(stdout, c, dump, encoded, base+" session "+s.ID)
	rep, err := replay.Run(ctx, &http.Client{Timeout: 30 * time.Second}, base, s.ID, s.Token, dump, replayOptions(c, stdout))
	if err != nil {
		return fail(err)
	}
	return reportReplay(stdout, fail, rep)
}

// runReplayInto relays a dump into a session on a running tower, which is how
// the dashboard is exercised without a camera.
func runReplayInto(ctx context.Context, c config, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	dump, encoded, err := beam.Load(c.replayFile)
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
	printReplayHeader(stdout, c, dump, encoded, base+" session "+sid)
	rep, err := replay.Run(ctx, client, base, sid, token, dump, replayOptions(c, stdout))
	if err != nil {
		return fail(err)
	}
	return reportReplay(stdout, fail, rep)
}

func replayOptions(c config, stdout io.Writer) replay.Options {
	return replay.Options{
		Rate:    c.rate,
		Drop:    c.drop,
		Shuffle: c.shuffle,
		Passes:  c.passes,
		Seed:    c.seed,
		Logf:    func(format string, args ...any) { fmt.Fprintf(stdout, "  "+format+"\n", args...) },
	}
}

func printReplayHeader(stdout io.Writer, c config, dump *beam.Dump, encoded bool, target string) {
	m := dump.Manifest
	note := ""
	if encoded {
		note = " (not a frames dump: encoded on the fly)"
	}
	fmt.Fprintf(stdout, "replay %s%s → %s\n", c.replayFile, note, target)
	fmt.Fprintf(stdout, "  %d chunks × %d bytes, %d → %d bytes gzip, sender session 0x%08x\n",
		m.Total(), m.Chunk, m.OrigSize, m.GzSize, dump.SenderSession)
	fmt.Fprintf(stdout, "  rate %g fps, drop %.0f%%, shuffle %v, passes ≤ %d, seed %d\n",
		c.rate, c.drop*100, c.shuffle, c.passes, c.seed)
}

func reportReplay(stdout io.Writer, fail func(error) int, rep *replay.Report) int {
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

// printJoin shows the join link and its QR. The token is part of the link by
// design; it goes to the operator's terminal, never to the log.
func printJoin(w io.Writer, sid, join string) {
	fmt.Fprintf(w, "\nsession %s\njoin    %s\n\n", sid, join)
	code, err := terminalQR(join)
	if err != nil {
		fmt.Fprintf(w, "(no QR: %v)\n", err)
		return
	}
	io.WriteString(w, code)
}
