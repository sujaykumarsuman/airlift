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
	cfg, err := config.Load(config.Params{Flags: flags, ConfigFile: *configFile, CreateFile: true})
	if err != nil {
		logger.Printf("error: %v", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, cfg, *headless, stdout, logger)
}

func runServe(ctx context.Context, cfg *config.Config, headless bool, stdout io.Writer, logger *log.Logger) int {
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

	store := session.NewStore(cfg.InactiveTTL, cfg.Sessions)
	store.SetLimits(cfg.MaxBeams, cfg.MaxGzBytes)
	store.SetLifecycle(cfg.IdleTTL, cfg.InactiveTTL, cfg.MaxAge, cfg.TerminatedTTL)
	go store.Run(ctx, sweepEvery)
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
			MaxGzBytes:  cfg.MaxGzBytes,
			IdleTTL:     cfg.IdleTTL,
			InactiveTTL: cfg.InactiveTTL,
			MaxAge:      cfg.MaxAge,
			Sessions:    cfg.Sessions,
		},
		MaxBody:    cfg.MaxBody,
		RateCreate: server.Rate(cfg.RateCreate),
		RateJoin:   server.Rate(cfg.RateJoin),
		RateFrames: server.Rate(cfg.RateFrames),
		RatePing:   server.Rate(cfg.RatePing),
		OnCreate:   func(s *session.Session, join string) { printJoin(stdout, s.ID, join) },
		Logf:       logger.Printf,
	})

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
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdownCtx); err != nil {
			hs.Close()
		}
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fail(err)
	}
	logger.Printf("stopped")
	return 0
}

// prepareDataDir creates data_dir and empties it on start (decision 9), but
// only after refusing dangerous targets and any pre-existing directory airlift
// did not create — a misconfigured data_dir must never wipe the host.
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
				return fmt.Errorf("data_dir %s is not empty and not an airlift data directory; refusing to erase it", abs)
			}
		}
		if err := os.RemoveAll(abs); err != nil {
			return err
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
