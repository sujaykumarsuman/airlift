package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/bundle"
	"github.com/sujaykumarsuman/airlift/internal/proto"
)

const beamUsage = `usage: airlift beam [PATH...] [flags]

  Bundles a folder (git-aware), takes one file as it is, or bundles several
  files (they need a name), then either writes a QR page to show on this
  machine's screen — the air-gapped way — or, on a machine that is not
  air-gapped, sends it straight to a session with --to-session. Run it with no
  PATH on a terminal and it asks what to beam and where.

what to send
  --name NAME                       beam name (default: the folder or file name)
  --files-from LIST                 read paths from LIST, one per line ("name: X" sets the name)
  --format auto|text|base64         bundle format (default auto: text unless a file is binary)
  --mode auto|sequential|fountain   frame layout (default auto)
  --chunk BYTES                     payload bytes per frame (a page: what QR version 30
                                    holds at --ecc; a session: 2712, the most a frame carries)

a QR page (the default)
  --out FILE                        where to write it (default <name>.html)
  --no-open                         do not open it in a browser
  --fps N                           initial frames per second, 1..60 (default 10)
  --ecc L|M|Q|H                     QR error correction (default M)
  --version-target V                the largest chunk QR version V (1..40) holds
  --manifest-every K                re-insert the manifest every K frames (default 20)
  --seed N                          derive the sender id from N (reproducible output)

straight to a session (not air-gapped)
  -s, --to-session LINK             the session's link from its dashboard; without its
                                    token you are asked for the password, or a session
                                    admin is asked to let you in
  --wait DURATION                   how long to wait for a session admin to let you in and
                                    to approve the upload (default 3m)
`

func cmdBeam(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("beam", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, beamUsage) }
	name := fs.String("name", "", "")
	filesFrom := fs.String("files-from", "", "")
	format := fs.String("format", "auto", "")
	mode := fs.String("mode", "auto", "")
	chunk := fs.Int("chunk", 0, "")
	out := fs.String("out", "", "")
	noOpen := fs.Bool("no-open", false, "")
	fps := fs.Int("fps", beam.DefaultFPS, "")
	ecc := fs.String("ecc", "M", "")
	versionTarget := fs.Int("version-target", 0, "")
	manifestEvery := fs.Int("manifest-every", 20, "")
	seed := fs.Int64("seed", 0, "")
	toSession := fs.String("to-session", "", "")
	fs.StringVar(toSession, "s", "", "")
	wait := fs.Duration("wait", 3*time.Minute, "")
	positional, err := parsePermuted(fs, args)
	if err != nil {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	seedPtr := (*int64)(nil)
	if set["seed"] {
		seedPtr = seed
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "airlift beam: %v\n", err)
		return 2
	}
	if *fps < 1 || *fps > 60 {
		return fail(errors.New("--fps must be 1..60"))
	}
	if *manifestEvery < 1 {
		return fail(errors.New("--manifest-every must be >= 1"))
	}
	if *format != "auto" && *format != "text" && *format != "base64" {
		return fail(errors.New("--format must be auto, text or base64"))
	}
	layout, ok := map[string]beam.Mode{"auto": beam.ModeAuto, "sequential": beam.ModeSequential, "fountain": beam.ModeFountain}[*mode]
	if !ok {
		return fail(errors.New("--mode must be auto, sequential or fountain"))
	}
	if *wait <= 0 {
		return fail(errors.New("--wait must be a positive duration, like 90s or 5m"))
	}
	ask := newPrompter(stderr)
	st := newStatus(stderr)

	inputs := append([]string(nil), positional...)
	listName := ""
	if *filesFrom != "" {
		listPaths, n, err := readFileList(*filesFrom)
		if err != nil {
			return fail(err)
		}
		inputs = append(inputs, listPaths...)
		listName = n
	}
	if len(inputs) == 0 {
		if !ask.interactive {
			return fail(errors.New("give a folder, a file, or file paths to beam"))
		}
		// Nothing given on a terminal: ask, rather than print usage at someone
		// who has just started.
		fmt.Fprintln(stderr, "airlift beam — nothing given, so two questions (Ctrl-C to stop):")
		if inputs, err = askPaths(ask); err != nil {
			return fail(err)
		}
		if !set["to-session"] && !set["s"] && !set["out"] {
			line, err := ask.ask("where", "a session link to send it to, or Enter for a QR page")
			if err != nil {
				return fail(err)
			}
			*toSession = line
		}
	}

	direct := *toSession != ""
	var link sessionLink
	if direct {
		if link, err = parseSessionLink(*toSession); err != nil {
			return fail(err)
		}
		if set["out"] {
			return fail(errors.New("--out writes a QR page and --to-session sends straight to a session: use one or the other"))
		}
		var ignored []string
		for _, f := range []string{"no-open", "fps", "ecc", "version-target", "manifest-every", "seed"} {
			if set[f] {
				ignored = append(ignored, "--"+f)
			}
		}
		if len(ignored) > 0 {
			fmt.Fprintf(stderr, "airlift beam: ignoring page-only flags with --to-session: %s\n", strings.Join(ignored, ", "))
		}
		seedPtr = nil // a fixed sender id could name a beam already in the session
	}

	data, beamName, bundleFormat, err := resolveBeam(inputs, *name, listName, *format, stderr, ask, st)
	st.clear()
	if err != nil {
		return fail(err)
	}

	ch := *chunk
	switch {
	case direct && ch == 0:
		ch = beam.MaxChunk // over HTTP a frame's size costs nothing to decode
	case direct:
	case *versionTarget != 0:
		if ch, err = beam.ChunkForVersion(*versionTarget, *ecc); err != nil {
			return fail(err)
		}
	case ch == 0:
		// The default is a version-30 symbol whatever the ECC: 1311 bytes at M,
		// more at L, fewer at Q and H.
		if ch, err = beam.ChunkForVersion(30, *ecc); err != nil {
			return fail(err)
		}
	}

	if direct {
		if *mode == "auto" {
			layout = beam.ModeSequential // HTTP loses nothing, so a fountain's surplus buys nothing
		}
		d, err := beam.EncodeWith(data, beamName, ch, beam.NewSession(seedPtr), layout, 0, compressProgress(st))
		st.clear()
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "airlift beam  %s → session %s\n", beamName, link.SID)
		packets := 0
		if d.Fountain != nil {
			packets = d.Fountain.Packets
		}
		printPayload(stdout, d.Manifest, bundleFormat, ch, packets)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
		defer stop()
		ask.ctx = ctx // Ctrl-C ends a question too, echo restored
		job := &sendJob{link: link, dump: d, name: beamName, wait: *wait, out: stdout, st: st, ask: ask}
		err = job.run(ctx)
		st.clear()
		withdrawn := ""
		if job.withdrew {
			withdrawn = "; the upload request was withdrawn"
		}
		switch {
		case err == nil:
			return 0
		case ctx.Err() != nil:
			fmt.Fprintf(stderr, "airlift beam: interrupted%s\n", withdrawn)
			return 130
		}
		if withdrawn != "" && !strings.Contains(err.Error(), "withdrawn") {
			fmt.Fprintf(stderr, "airlift beam: %v%s\n", err, withdrawn)
		} else {
			fmt.Fprintf(stderr, "airlift beam: %v\n", err)
		}
		return 1
	}

	started := time.Now()
	res, err := beam.Build(data, beamName, beam.Options{
		Chunk: ch, ECC: *ecc, FPS: *fps, ManifestEvery: *manifestEvery, Seed: seedPtr, Mode: layout,
		Progress: compressProgress(st),
	})
	st.clear()
	if err != nil {
		return fail(err)
	}
	outPath := *out
	if outPath == "" {
		outPath = htmlFilename(beamName)
	}
	if err := os.WriteFile(outPath, []byte(res.HTML), 0o644); err != nil {
		return fail(err)
	}
	printBeamSummary(stdout, beamName, outPath, res, bundleFormat, ch, *ecc, *fps, *manifestEvery, time.Since(started))
	if !*noOpen {
		if err := openBrowser(outPath); err != nil {
			fmt.Fprintf(stdout, "  open     %s  (could not launch a browser: %v)\n", outPath, err)
		} else {
			fmt.Fprintf(stdout, "  opened   %s\n", outPath)
		}
	}
	return 0
}

// askPaths asks what to beam until the answer names paths that exist.
func askPaths(ask *prompter) ([]string, error) {
	for {
		line, err := ask.ask("what", "a folder or files to beam")
		if err != nil {
			return nil, err
		}
		paths, err := splitPaths(line)
		if err != nil {
			fmt.Fprintf(ask.out, "  %v — try again\n", err)
			continue
		}
		missing := ""
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				missing = p
				break
			}
		}
		switch {
		case len(paths) == 0:
		case missing != "":
			fmt.Fprintf(ask.out, "  no such file or folder: %s\n", missing)
		default:
			return paths, nil
		}
	}
}

// compressProgress draws gzip's progress for a payload big enough to wait on.
func compressProgress(st *status) func(done, total int64) {
	start := time.Now()
	return func(done, total int64) {
		if total < 8<<20 {
			return
		}
		st.live(fmt.Sprintf("  gzip     %s %3d %%  %s of %s  %s", bar(done, total, 20), 100*done/total, humanBytes(done), humanBytes(total), rate(done, time.Since(start))))
	}
}

// resolveBeam turns the input paths into the payload bytes and the beam name.
// One directory is bundled (git-aware); one file is sent as-is; several files
// are bundled, rooted at the working directory, and need a name — from --name,
// a `name:` line in --files-from, or a prompt on a terminal. The third result
// is the bundle format written ("" for a single file, sent as-is).
func resolveBeam(inputs []string, flagName, listName, format string, stderr io.Writer, ask *prompter, st *status) ([]byte, string, string, error) {
	name := flagName
	if name == "" {
		name = listName
	}
	if len(inputs) == 1 {
		info, err := os.Stat(inputs[0])
		if err != nil {
			return nil, "", "", err
		}
		if info.IsDir() {
			if name == "" {
				abs, err := filepath.Abs(inputs[0])
				if err != nil {
					return nil, "", "", err
				}
				name = filepath.Base(abs)
			}
			var buf bytes.Buffer
			used, err := packBundle(&buf, inputs[0], format, nil, "", stderr, st)
			if err != nil {
				return nil, "", "", err
			}
			return buf.Bytes(), name, used, nil
		}
		if !info.Mode().IsRegular() {
			return nil, "", "", fmt.Errorf("%s is not a regular file", inputs[0])
		}
		if name == "" {
			name = filepath.Base(inputs[0])
		}
		data, err := os.ReadFile(inputs[0])
		return data, name, "", err
	}
	if name == "" {
		line, err := ask.ask("name", "a name for these files")
		switch {
		case errors.Is(err, errNotInteractive):
			return nil, "", "", errors.New("several files need a name: pass --name NAME")
		case err != nil:
			return nil, "", "", err
		case line == "":
			return nil, "", "", errors.New("a name is required: pass --name NAME")
		}
		name = line
	}
	abs := make([]string, len(inputs))
	for i, p := range inputs {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, "", "", err
		}
		abs[i] = a
	}
	root := commonDir(abs)
	explicit, err := bundle.ResolveExplicit(root, abs)
	if err != nil {
		return nil, "", "", err
	}
	var buf bytes.Buffer
	used, err := packBundle(&buf, root, format, explicit, "", stderr, st)
	if err != nil {
		return nil, "", "", err
	}
	return buf.Bytes(), name, used, nil
}

// packBundle writes the bundle in the requested format and reports the one
// written. "auto" tries text — the smaller, human-readable form, about 30 %
// less gzip than base64 for source trees — and falls back to base64 when a
// file is binary (text would drop it) or holds a boundary marker (text
// refuses it), so nothing is ever silently left out of a beam.
func packBundle(buf *bytes.Buffer, root, format string, explicit []string, excludeAbs string, stderr io.Writer, st *status) (string, error) {
	w := &countingWriter{w: buf, report: func(n int64) { st.live("  bundle   reading files · " + humanBytes(n)) }}
	if format != "auto" {
		rep, err := bundle.Pack(w, root, format, explicit, excludeAbs)
		if err == nil && len(rep.Skipped) > 0 {
			// An explicit text bundle drops binaries by design; say so rather than
			// letting a file go missing quietly.
			st.clear()
			fmt.Fprintf(stderr, "airlift beam: text format skipped %d binary file(s): %s\n", len(rep.Skipped), strings.Join(rep.Skipped, ", "))
		}
		return format, err
	}
	rep, err := bundle.Pack(w, root, "text", explicit, excludeAbs)
	if err == nil && len(rep.Skipped) == 0 {
		return "text", nil
	}
	if err != nil && !errors.Is(err, bundle.ErrBoundary) {
		return "", err
	}
	buf.Reset()
	w.n = 0
	_, err = bundle.Pack(w, root, "base64", explicit, excludeAbs)
	return "base64", err
}

// commonDir is the deepest directory that contains every absolute file path,
// so a set of scattered files bundles with the shortest sensible relative
// paths. With one file it is that file's directory.
func commonDir(absFiles []string) string {
	sep := string(filepath.Separator)
	common := strings.Split(filepath.Dir(absFiles[0]), sep)
	for _, f := range absFiles[1:] {
		parts := strings.Split(filepath.Dir(f), sep)
		n := min(len(common), len(parts))
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
	}
	root := strings.Join(common, sep)
	if root == "" {
		root = sep
	}
	return root
}

// htmlFilename is the default --out: the beam name with its extension replaced
// by .html (a folder name has none), reduced to a single path component.
func htmlFilename(name string) string {
	base := filepath.Base(name)
	if base == "." || base == string(filepath.Separator) {
		base = "beam"
	}
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base + ".html"
}

func printBeamSummary(w io.Writer, beamName, outPath string, res *beam.Result, bundleFormat string, chunk int, ecc string, fps, manifestEvery int, took time.Duration) {
	modules := 17 + 4*res.Version
	fmt.Fprintf(w, "airlift beam  %s → %s\n", beamName, outPath)
	printPayload(w, res.Dump.Manifest, bundleFormat, chunk, res.Packets)
	fmt.Fprintf(w, "  qr       version %d (%d×%d modules), ECC %s, alphanumeric\n", res.Version, modules, modules, ecc)
	fmt.Fprintf(w, "  loop     %10d frames   manifest every %d   %.1f s per pass at %d fps\n", len(res.Order), manifestEvery, float64(len(res.Order))/float64(fps), fps)
	fmt.Fprintf(w, "  session  0x%08x\n", res.Dump.SenderSession)
	fmt.Fprintf(w, "  output   %s   built in %s\n", human(len(res.HTML)), elapsed(took))
}

// printPayload is the part of a summary both destinations share: what went
// in, how it compressed, and how it was cut into frames (packets > 0 for a
// fountain layout).
func printPayload(w io.Writer, m proto.Manifest, bundleFormat string, chunk, packets int) {
	ratio := 0.0
	if m.OrigSize > 0 {
		ratio = 100.0 * float64(m.GzSize) / float64(m.OrigSize)
	}
	fmt.Fprintf(w, "  input    %10d bytes   sha256 %s…\n", m.OrigSize, m.OrigSHA256[:16])
	if bundleFormat != "" {
		fmt.Fprintf(w, "  bundle   %s format\n", bundleFormat)
	}
	fmt.Fprintf(w, "  gzip     %10d bytes   %.1f %% of input\n", m.GzSize, ratio)
	fmt.Fprintf(w, "  chunks   %10d × %d bytes\n", m.Total(), chunk)
	if packets > 0 {
		fmt.Fprintf(w, "  mode     fountain (%d packets, LT/robust soliton)\n", packets)
	} else {
		fmt.Fprintf(w, "  mode     sequential\n")
	}
}
