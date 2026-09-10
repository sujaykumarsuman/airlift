package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sujaykumarsuman/airlift/internal/beam"
	"github.com/sujaykumarsuman/airlift/internal/bundle"
)

func cmdBeam(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("beam", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift beam PATH [PATH...] [--name NAME] [--out HTML] [--no-open] [flags]\n\n"+
			"  one folder is bundled; one file is sent as-is; several files are bundled\n"+
			"  and need --name. The page is self-contained and opens in your browser.\n\n")
		fs.PrintDefaults()
	}
	name := fs.String("name", "", "beam name (defaults to the folder or file name; required for several files)")
	format := fs.String("format", "base64", "bundle format for a folder or several files: base64 or text")
	filesFrom := fs.String("files-from", "", "read the file list from this file (one path per line; a `name: X` line sets the name)")
	out := fs.String("out", "", "output HTML file (default <name>.html)")
	noOpen := fs.Bool("no-open", false, "do not open the beam in a browser")
	chunk := fs.Int("chunk", beam.DefaultChunk, "payload bytes per frame (default 600; at most 2242 at ECC M)")
	ecc := fs.String("ecc", "M", "QR error-correction level: L, M, Q or H")
	versionTarget := fs.Int("version-target", 0, "pick the largest chunk that fits QR version V (1..40) instead of --chunk")
	fps := fs.Int("fps", 5, "initial frames per second (1..60)")
	manifestEvery := fs.Int("manifest-every", 20, "re-insert the manifest frame after every K frames")
	seed := fs.Int64("seed", 0, "derive the sender session id from N instead of at random")
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
	if *fps < 1 || *fps > 60 {
		return fail(errors.New("--fps must be 1..60"))
	}
	if *manifestEvery < 1 {
		return fail(errors.New("--manifest-every must be >= 1"))
	}
	if *format != "text" && *format != "base64" {
		return fail(errors.New("--format must be text or base64"))
	}

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
		return fail(errors.New("give a folder, a file, or file paths to beam"))
	}

	data, beamName, err := resolveBeam(inputs, *name, listName, *format, stderr)
	if err != nil {
		return fail(err)
	}

	ch := *chunk
	if *versionTarget != 0 {
		if ch, err = beam.ChunkForVersion(*versionTarget, *ecc); err != nil {
			return fail(err)
		}
	}
	res, err := beam.Build(data, beamName, beam.Options{
		Chunk: ch, ECC: *ecc, FPS: *fps, ManifestEvery: *manifestEvery, Seed: seedPtr, Mode: beam.ModeAuto,
	})
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
	printBeamSummary(stdout, beamName, outPath, res, ch, *ecc, *fps, *manifestEvery)
	if !*noOpen {
		if err := openBrowser(outPath); err != nil {
			fmt.Fprintf(stdout, "  open     %s  (could not launch a browser: %v)\n", outPath, err)
		} else {
			fmt.Fprintf(stdout, "  opened   %s\n", outPath)
		}
	}
	return 0
}

// resolveBeam turns the input paths into the payload bytes and the beam name.
// One directory is bundled (git-aware); one file is sent as-is; several files
// are bundled, rooted at the working directory, and need a name — from --name,
// a `name:` line in --files-from, or a prompt on a terminal.
func resolveBeam(inputs []string, flagName, listName, format string, stderr io.Writer) ([]byte, string, error) {
	name := flagName
	if name == "" {
		name = listName
	}
	if len(inputs) == 1 {
		info, err := os.Stat(inputs[0])
		if err != nil {
			return nil, "", err
		}
		if info.IsDir() {
			if name == "" {
				abs, err := filepath.Abs(inputs[0])
				if err != nil {
					return nil, "", err
				}
				name = filepath.Base(abs)
			}
			var buf bytes.Buffer
			if _, err := bundle.Pack(&buf, inputs[0], format, nil, ""); err != nil {
				return nil, "", err
			}
			return buf.Bytes(), name, nil
		}
		if !info.Mode().IsRegular() {
			return nil, "", fmt.Errorf("%s is not a regular file", inputs[0])
		}
		if name == "" {
			name = filepath.Base(inputs[0])
		}
		data, err := os.ReadFile(inputs[0])
		return data, name, err
	}
	if name == "" {
		var err error
		if name, err = promptName(stderr); err != nil {
			return nil, "", err
		}
	}
	abs := make([]string, len(inputs))
	for i, p := range inputs {
		a, err := filepath.Abs(p)
		if err != nil {
			return nil, "", err
		}
		abs[i] = a
	}
	root := commonDir(abs)
	explicit, err := bundle.ResolveExplicit(root, abs)
	if err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	if _, err := bundle.Pack(&buf, root, format, explicit, ""); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), name, nil
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

// promptName asks for a beam name on a terminal; off a terminal it is an error
// telling the caller to pass --name.
func promptName(stderr io.Writer) (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("several files need a name: pass --name NAME")
	}
	fmt.Fprint(stderr, "beam name: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	name := strings.TrimSpace(line)
	if name == "" {
		return "", errors.New("a name is required: pass --name NAME")
	}
	return name, nil
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

func printBeamSummary(w io.Writer, beamName, outPath string, res *beam.Result, chunk int, ecc string, fps, manifestEvery int) {
	m := res.Dump.Manifest
	ratio := 0.0
	if m.OrigSize > 0 {
		ratio = 100.0 * float64(m.GzSize) / float64(m.OrigSize)
	}
	modules := 17 + 4*res.Version
	fmt.Fprintf(w, "airlift beam  %s → %s\n", beamName, outPath)
	fmt.Fprintf(w, "  input    %10d bytes   sha256 %s…\n", m.OrigSize, m.OrigSHA256[:16])
	fmt.Fprintf(w, "  gzip     %10d bytes   %.1f %% of input\n", m.GzSize, ratio)
	fmt.Fprintf(w, "  chunks   %10d × %d bytes\n", m.Total(), chunk)
	if res.Fountain {
		fmt.Fprintf(w, "  mode     fountain (%d packets, LT/robust soliton)\n", res.Packets)
	} else {
		fmt.Fprintf(w, "  mode     sequential\n")
	}
	fmt.Fprintf(w, "  qr       version %d (%d×%d modules), ECC %s, alphanumeric\n", res.Version, modules, modules, ecc)
	fmt.Fprintf(w, "  loop     %10d frames   manifest every %d   %.1f s per pass at %d fps\n", len(res.Order), manifestEvery, float64(len(res.Order))/float64(fps), fps)
	fmt.Fprintf(w, "  session  0x%08x\n", res.Dump.SenderSession)
	fmt.Fprintf(w, "  output   %s\n", human(len(res.HTML)))
}
