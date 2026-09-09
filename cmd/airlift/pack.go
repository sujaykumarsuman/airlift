package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
)

func cmdPack(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift pack [PATHS...] [--root DIR] [--out FILE] [--format text|base64] [--files-from LIST]\n\n")
		fs.PrintDefaults()
	}
	root := fs.String("root", ".", "directory the paths are relative to and the bundle is rooted at")
	out := fs.String("out", "repo-bundle.txt", "output bundle file")
	format := fs.String("format", "text", "text (human-readable, skips binaries) or base64 (copy-paste-proof, includes binaries)")
	filesFrom := fs.String("files-from", "", "read the file list from this file, one path per line (# comments allowed)")
	paths, err := parsePermuted(fs, args)
	if err != nil {
		return 2
	}
	if *format != "text" && *format != "base64" {
		fmt.Fprintln(stderr, "airlift pack: --format must be text or base64")
		return 2
	}
	rootAbs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(stderr, "airlift pack: %v\n", err)
		return 1
	}
	if *filesFrom != "" {
		extra, err := readFileList(*filesFrom)
		if err != nil {
			fmt.Fprintf(stderr, "airlift pack: %v\n", err)
			return 1
		}
		paths = append(paths, extra...)
	}
	var explicit []string
	if len(paths) > 0 {
		if explicit, err = bundle.ResolveExplicit(rootAbs, paths); err != nil {
			fmt.Fprintf(stderr, "airlift pack: %v\n", err)
			return 1
		}
	}
	outAbs, err := filepath.Abs(*out)
	if err != nil {
		fmt.Fprintf(stderr, "airlift pack: %v\n", err)
		return 1
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintf(stderr, "airlift pack: %v\n", err)
		return 1
	}
	rep, perr := bundle.Pack(f, rootAbs, *format, explicit, outAbs)
	cerr := f.Close()
	if perr != nil {
		fmt.Fprintf(stderr, "airlift pack: %v\n", perr)
		return 1
	}
	if cerr != nil {
		fmt.Fprintf(stderr, "airlift pack: %v\n", cerr)
		return 1
	}
	size := int64(-1)
	if info, err := os.Stat(*out); err == nil {
		size = info.Size()
	}
	fmt.Fprintf(stdout, "packed %d files -> %s (%d bytes, format=%s, %s)\n", rep.Packed, *out, size, *format, rep.Source)
	if len(rep.Skipped) > 0 {
		fmt.Fprintf(stdout, "skipped %d binary file(s) (use --format base64 to include them):\n", len(rep.Skipped))
		for _, r := range rep.Skipped {
			fmt.Fprintf(stdout, "  - %s\n", r)
		}
	}
	return 0
}
