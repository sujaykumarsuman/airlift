package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sujaykumarsuman/airlift/internal/bundle"
)

func cmdUnpack(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unpack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift unpack [--in FILE] [--dest DIR] [--dry-run]\n\n")
		fs.PrintDefaults()
	}
	in := fs.String("in", "repo-bundle.txt", "input bundle file")
	dest := fs.String("dest", ".", "directory to restore into")
	dry := fs.Bool("dry-run", false, "verify every entry without writing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(stderr, "airlift unpack: %v\n", err)
		return 1
	}
	b, err := bundle.Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "airlift unpack: %v\n", err)
		return 1
	}
	var ok []bundle.File
	var bad []string
	for _, f := range b.Files {
		mark := "OK "
		if f.OK {
			ok = append(ok, f)
		} else {
			mark = "BAD"
			bad = append(bad, f.Path)
		}
		fmt.Fprintf(stdout, "  %s  %s\n", mark, f.Path)
	}
	if !*dry {
		if err := bundle.WriteTree(*dest, ok); err != nil {
			fmt.Fprintf(stderr, "airlift unpack: %v\n", err)
			return 1
		}
	}
	prefix := ""
	if *dry {
		prefix = "(dry-run) "
	}
	fmt.Fprintf(stdout, "%srestored %d files into %s\n", prefix, len(ok), *dest)
	if len(bad) > 0 {
		fmt.Fprintf(stderr, "WARNING: %d file(s) failed verification (copy-paste corruption or an unsafe path): %v\n", len(bad), bad)
		return 1
	}
	return 0
}
