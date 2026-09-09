package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/bundle"
)

func cmdBeam(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("beam", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift beam (--in FILE | --root DIR [PATHS...]) [--out HTML] [--dump JSON] [flags]\n\n")
		fs.PrintDefaults()
	}
	in := fs.String("in", "", "file to send as-is")
	root := fs.String("root", "", "pack this folder (with any PATHS or --files-from) and beam the bundle")
	filesFrom := fs.String("files-from", "", "with --root: read the file list from this file")
	format := fs.String("format", "base64", "with --root: bundle format, base64 or text")
	noBundle := fs.Bool("no-bundle", false, "with --root: do not also write the bundle next to the page")
	out := fs.String("out", "beam.html", "output HTML file")
	dump := fs.String("dump", "", "also write the frames dump to this JSON file")
	chunk := fs.Int("chunk", beam.DefaultChunk, "payload bytes per frame (default 600; at most 2242 at ECC M)")
	ecc := fs.String("ecc", "M", "QR error-correction level: L, M, Q or H")
	versionTarget := fs.Int("version-target", 0, "pick the largest chunk that fits QR version V (1..40) instead of --chunk")
	fps := fs.Int("fps", 8, "initial frames per second (1..60)")
	manifestEvery := fs.Int("manifest-every", 20, "re-insert the manifest frame after every K frames")
	seed := fs.Int64("seed", 0, "derive the sender session id from N instead of at random")
	fountain := fs.Bool("fountain", false, "emit LT fountain packets instead of sequential chunks")
	fountainPackets := fs.Int("fountain-packets", 0, "fountain packets to emit (default N + max(48, 3·√N·lnN))")
	positional, err := parsePermuted(fs, args)
	if err != nil {
		return 2
	}
	seedPtr := (*int64)(nil)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedPtr = seed
		}
	})
	fail := func(err error) int {
		fmt.Fprintf(stderr, "airlift beam: %v\n", err)
		return 2
	}
	if (*in == "") == (*root == "") {
		return fail(fmt.Errorf("give exactly one of --in FILE or --root DIR"))
	}
	if *fps < 1 || *fps > 60 {
		return fail(fmt.Errorf("--fps must be 1..60"))
	}
	if *manifestEvery < 1 {
		return fail(fmt.Errorf("--manifest-every must be >= 1"))
	}

	var data []byte
	var name string
	if *in != "" {
		if len(positional) > 0 {
			return fail(fmt.Errorf("PATHS are only for --root; got %v with --in", positional))
		}
		b, err := os.ReadFile(*in)
		if err != nil {
			return fail(err)
		}
		data, name = b, filepath.Base(*in)
	} else {
		b, n, err := packTree(*root, *format, positional, *filesFrom, *out, *noBundle, stdout)
		if err != nil {
			return fail(err)
		}
		data, name = b, n
	}

	fmt.Fprintf(stdout, "airlift beam  %s → %s\n", name, *out)
	ch := *chunk
	if *versionTarget != 0 {
		c, err := beam.ChunkForVersion(*versionTarget, *ecc)
		if err != nil {
			return fail(err)
		}
		ch = c
		fmt.Fprintf(stdout, "  target   QR version %d at ECC %s → chunk %d bytes\n", *versionTarget, *ecc, ch)
	}
	session := beam.NewSession(seedPtr)
	d, err := beam.Encode(data, name, ch, session, *fountain, *fountainPackets)
	if err != nil {
		return fail(err)
	}
	version, size, paths, err := beam.RenderQR(d.Frames, *ecc)
	if err != nil {
		return fail(err)
	}
	order := beam.Schedule(len(d.Frames)-1, *manifestEvery)
	html := beam.PlayerHTML(name, session, len(d.Frames)-1, order, paths, size, *fps, *fountain)
	if err := os.WriteFile(*out, []byte(html), 0o644); err != nil {
		return fail(err)
	}
	if *dump != "" {
		if err := writeDump(*dump, d); err != nil {
			return fail(err)
		}
	}
	printBeamSummary(stdout, d, ch, version, order, *ecc, *fps, *manifestEvery, *fountain, len(html))
	return 0
}

// packTree packs root (with any explicit paths / --files-from) to a bundle,
// writing it next to the page unless noBundle, and returns the bundle bytes and
// the name carried in the manifest.
func packTree(root, format string, paths []string, filesFrom, out string, noBundle bool, stdout io.Writer) ([]byte, string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, "", err
	}
	if filesFrom != "" {
		extra, err := readFileList(filesFrom)
		if err != nil {
			return nil, "", err
		}
		paths = append(append([]string(nil), paths...), extra...)
	}
	var explicit []string
	if len(paths) > 0 {
		if explicit, err = bundle.ResolveExplicit(rootAbs, paths); err != nil {
			return nil, "", err
		}
	}
	name := filepath.Base(filepath.Clean(rootAbs)) + ".bundle.txt"
	bundlePath := filepath.Join(filepath.Dir(out), name)
	bundleAbs, err := filepath.Abs(bundlePath)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	rep, err := bundle.Pack(&buf, rootAbs, format, explicit, bundleAbs)
	if err != nil {
		return nil, "", err
	}
	if !noBundle {
		if err := os.WriteFile(bundlePath, buf.Bytes(), 0o644); err != nil {
			return nil, "", err
		}
		fmt.Fprintf(stdout, "airlift pack   %d files → %s (%s, %s)\n", rep.Packed, bundlePath, format, rep.Source)
		for _, r := range rep.Skipped {
			fmt.Fprintf(stdout, "  skipped binary %s\n", r)
		}
	}
	return buf.Bytes(), name, nil
}

func printBeamSummary(w io.Writer, d *beam.Dump, chunk, version int, order []int, ecc string, fps, manifestEvery int, fountain bool, docBytes int) {
	m := d.Manifest
	ratio := 0.0
	if m.OrigSize > 0 {
		ratio = 100.0 * float64(m.GzSize) / float64(m.OrigSize)
	}
	modules := 17 + 4*version
	fmt.Fprintf(w, "  input    %10d bytes   sha256 %s…\n", m.OrigSize, m.OrigSHA256[:16])
	fmt.Fprintf(w, "  gzip     %10d bytes   %.1f %% of input\n", m.GzSize, ratio)
	fmt.Fprintf(w, "  chunks   %10d × %d bytes\n", m.Total(), chunk)
	if fountain {
		fmt.Fprintf(w, "  fountain %10d packets   (LT, robust soliton)\n", len(d.Frames)-1)
	}
	fmt.Fprintf(w, "  qr       version %d (%d×%d modules), ECC %s, alphanumeric\n", version, modules, modules, ecc)
	fmt.Fprintf(w, "  loop     %10d frames   manifest every %d   %.1f s per pass at %d fps\n", len(order), manifestEvery, float64(len(order))/float64(fps), fps)
	fmt.Fprintf(w, "  session  0x%08x\n", d.SenderSession)
	fmt.Fprintf(w, "  output   %s\n", human(docBytes))
}
