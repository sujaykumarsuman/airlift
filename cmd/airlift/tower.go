package main

import (
	"context"
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
	"strings"
	"syscall"
	"time"

	airlift "github.com/sujaykumarsuman/airlift"
	"github.com/sujaykumarsuman/airlift/internal/config"
	"github.com/sujaykumarsuman/airlift/internal/server"
	"github.com/sujaykumarsuman/airlift/internal/session"
)

const sweepEvery = 30 * time.Second

func cmdTower(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tower", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift tower [--config FILE] [--session] [--<key> VALUE ...]\n\n"+
			"  settings come from ~/.airlift/config; every config key is also a flag.\n\n")
		fs.PrintDefaults()
	}
	configFile := fs.String("config", "", "config file (default $AIRLIFT_HOME/config or ~/.airlift/config)")
	headless := fs.Bool("session", false, "create a session at start and print its join QR (headless use)")
	// Every config key is also a flag of the same name; an unset flag defaults
	// to "" and is ignored (only fs.Visit-set flags feed the flag layer).
	for _, name := range config.Keys() {
		fs.String(name, "", "config: "+name)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "airlift tower: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	logger := log.New(stderr, "", log.Ltime)

	known := map[string]bool{}
	for _, name := range config.Keys() {
		known[name] = true
	}
	flags := map[string]string{}
	fs.Visit(func(f *flag.Flag) {
		if known[f.Name] {
			flags[f.Name] = f.Value.String()
		}
	})
	params := config.Params{Flags: flags, ConfigFile: *configFile, CreateFile: true}
	cfg, err := config.Load(params)
	if err != nil {
		logger.Printf("error: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, cfg, params, *headless, stdout, logger)
}

func runServe(ctx context.Context, cfg *config.Config, params config.Params, headless bool, stdout io.Writer, logger *log.Logger) int {
	fail := func(err error) int {
		logger.Printf("error: %v", err)
		return 1
	}
	base, basePath, err := server.ParsePublicURL(cfg.PublicURL)
	if err != nil {
		return fail(err)
	}
	if err := prepareDataDir(cfg.DataDir, cfg.Home); err != nil {
		return fail(err)
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fail(fmt.Errorf("listen %s: %w", cfg.Listen, err))
	}
	web, err := fs.Sub(airlift.Dist, "web/dist")
	if err != nil {
		ln.Close()
		return fail(err)
	}

	store := session.NewStore(cfg.IdleTTL, cfg.Sessions)
	store.SetLimits(cfg.MaxBeams, cfg.MaxGzBytes)
	store.SetLifecycle(cfg.IdleTTL, cfg.MaxAge, cfg.TerminatedTTL)
	srv := server.New(server.Options{
		Store:          store,
		PublicBase:     base,
		BasePath:       basePath,
		Web:            web,
		DataDir:        cfg.DataDir,
		TrustedProxies: server.ParseTrustedProxies(cfg.TrustedProxies),
		AdminEnabled:   cfg.AdminEnabled(),
		Version:        airlift.Version,
		Caps: server.Caps{
			MaxGzBytes:     cfg.MaxGzBytes,
			MaxUploadBytes: cfg.MaxUploadBytes,
			IdleTTL:        cfg.IdleTTL,
			MaxAge:         cfg.MaxAge,
			Sessions:       cfg.Sessions,
		},
		MaxBody:       cfg.MaxBody,
		WarningTTL:    cfg.WarningTTL,
		ReviewTTL:     cfg.ReviewTTL,
		RateCreate:    server.Rate(cfg.RateCreate),
		RateJoin:      server.Rate(cfg.RateJoin),
		RateFrames:    server.Rate(cfg.RateFrames),
		RatePing:      server.Rate(cfg.RatePing),
		RateExtension: server.Rate(cfg.RateExtension),
		AdminToken:    cfg.AdminToken,
		RateAdmin:     server.Rate(cfg.RateAdmin),
		Config:        cfg,
		ConfigParams:  params,
		OnCreate:      func(s *session.Session, join string) { printJoin(stdout, s.ID, join) },
		Logf:          logger.Printf,
	})
	// The sessions an earlier run wrote down come back before anything is served
	// (ADR 0025), and the sweep then advances any clock that ran out meanwhile.
	restored, err := srv.Restore()
	if err != nil {
		ln.Close()
		return fail(fmt.Errorf("restoring sessions from %s: %w", cfg.DataDir, err))
	}
	if restored > 0 {
		logger.Printf("restored %d session(s) from %s", restored, cfg.DataDir)
	}
	go store.Run(ctx, sweepEvery)

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	admin := "disabled"
	if cfg.AdminEnabled() {
		admin = "enabled"
	}
	fmt.Fprintf(stdout, "airlift tower\n  public   %s/\n  listen   %s\n  data     %s\n  admin    %s\n",
		base, cfg.Listen, cfg.DataDir, admin)
	if headless {
		if _, _, err := srv.CreateSession(); err != nil {
			ln.Close()
			return fail(err)
		}
	}
	if err := serve(ctx, hs, srv, ln, cfg.DrainTimeout, logger); err != nil {
		return fail(err)
	}
	logger.Printf("stopped")
	return 0
}

// settleTimeout bounds the wait, after a drain ran out of time and cut what was
// still open, for those handlers to record what they took.
const settleTimeout = 5 * time.Second

// serve runs the HTTP server on ln until ctx ends, then drains: the tower
// refuses new work (readiness answers 503) while the requests in flight get up
// to drainTimeout to finish, then it stops listening; anything still open at
// the deadline is cut. Last, every session is written down for the next start
// (ADR 0025). It returns only once that is done — returning earlier would let
// the process exit under the requests it is meant to finish.
func serve(ctx context.Context, hs *http.Server, srv *server.Server, ln net.Listener, drainTimeout time.Duration, logger *log.Logger) error {
	hs.RegisterOnShutdown(srv.Drain)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		start := time.Now()
		srv.Drain()
		drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		// Keep listening while the requests in flight finish: a request that
		// still reaches this tower is answered (or refused with a Retry-After)
		// rather than finding the port closed.
		srv.WaitIdle(drainCtx)
		if err := hs.Shutdown(drainCtx); err != nil {
			logger.Printf("drain: %d request(s) still open after %s; cutting them", srv.Active(), drainTimeout)
			hs.Close()
			settle, cancel := context.WithTimeout(context.Background(), settleTimeout)
			defer cancel()
			srv.WaitIdle(settle)
		}
		srv.SaveAll()
		logger.Printf("drained in %s", time.Since(start).Round(time.Millisecond))
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}

// prepareDataDir creates data_dir, or adopts the one an earlier run left: its
// sessions are restored from it, and what none of them accounts for is deleted
// (ADR 0025; it used to be emptied). It first refuses dangerous targets and any
// pre-existing directory airlift did not create — a misconfigured data_dir must
// never have the host's files deleted.
func prepareDataDir(dir, home string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := checkDataDirSafe(abs, home); err != nil {
		return err
	}
	sentinel := filepath.Join(abs, ".airlift-data")
	entries, err := os.ReadDir(abs)
	switch {
	case err == nil:
		if len(entries) > 0 {
			if _, serr := os.Stat(sentinel); serr != nil {
				return fmt.Errorf("data_dir %s is not empty and not an airlift data directory; refusing to use it", abs)
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	return os.WriteFile(sentinel, []byte("airlift\n"), 0o644)
}

// checkDataDirSafe refuses the filesystem root, the user's home or any ancestor
// of it, and the airlift home (which holds the config) as a data_dir.
func checkDataDirSafe(abs, home string) error {
	real := abs
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		real = r
	}
	if real == string(filepath.Separator) {
		return errors.New("data_dir must not be the filesystem root")
	}
	if uh, err := os.UserHomeDir(); err == nil {
		uh := resolvePath(uh)
		if real == uh || isAncestor(real, uh) {
			return fmt.Errorf("data_dir %s is your home directory or an ancestor of it; refusing to erase it", real)
		}
	}
	if home != "" && real == resolvePath(home) {
		return fmt.Errorf("data_dir must not be the airlift home %s (it holds the config); use a subdirectory", real)
	}
	return nil
}

// resolvePath returns the absolute, symlink-resolved form of p (best effort),
// so guards compare like with like regardless of symlinked home layouts.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// isAncestor reports whether dir is a strict ancestor of target.
func isAncestor(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == "." {
		return false
	}
	return !strings.HasPrefix(rel, "..")
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
