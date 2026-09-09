package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sujaykumarsuman/airlift/internal/beam"
)

func cmdFrames(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("frames", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift frames --in FILE [--out JSON] [--chunk N] [--seed N] [--fountain [--fountain-packets K]]\n\n")
		fs.PrintDefaults()
	}
	in := fs.String("in", "", "file to encode")
	out := fs.String("out", "frames.json", "output JSON dump")
	chunk := fs.Int("chunk", beam.DefaultChunk, "payload bytes per frame")
	seed := fs.Int64("seed", 0, "derive the sender session id from N instead of at random")
	fountain := fs.Bool("fountain", false, "emit LT fountain packets instead of sequential chunks")
	fountainPackets := fs.Int("fountain-packets", 0, "fountain packets to emit (default N + max(48, 3·√N·lnN))")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(stderr, "airlift frames: --in FILE is required")
		return 2
	}
	seedPtr := (*int64)(nil)
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seedPtr = seed
		}
	})
	data, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(stderr, "airlift frames: %v\n", err)
		return 1
	}
	session := beam.NewSession(seedPtr)
	d, err := beam.Encode(data, filepath.Base(*in), *chunk, session, *fountain, *fountainPackets)
	if err != nil {
		fmt.Fprintf(stderr, "airlift frames: %v\n", err)
		return 2
	}
	if err := writeDump(*out, d); err != nil {
		fmt.Fprintf(stderr, "airlift frames: %v\n", err)
		return 1
	}
	kind := fmt.Sprintf("%d data frames", len(d.Frames)-1)
	if *fountain {
		kind = fmt.Sprintf("%d fountain packets", len(d.Frames)-1)
	}
	fmt.Fprintf(stdout, "wrote %s: manifest + %s (%d chunks × %d bytes), session 0x%08x\n",
		*out, kind, d.Manifest.Total(), *chunk, session)
	return 0
}
