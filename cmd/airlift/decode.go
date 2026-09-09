package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sujaykumarsuman/airlift/internal/beam"
)

func cmdDecode(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: airlift decode --frames JSON --out FILE\n\n")
		fs.PrintDefaults()
	}
	framesPath := fs.String("frames", "", "frames dump written by `airlift frames` or `airlift beam --dump`")
	out := fs.String("out", "", "reassembled output file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *framesPath == "" || *out == "" {
		fmt.Fprintln(stderr, "airlift decode: --frames and --out are required")
		return 2
	}
	raw, err := os.ReadFile(*framesPath)
	if err != nil {
		fmt.Fprintf(stderr, "airlift decode: %v\n", err)
		return 1
	}
	var dump struct {
		Frames []string `json:"frames"`
	}
	if err := json.Unmarshal(raw, &dump); err != nil || len(dump.Frames) == 0 {
		fmt.Fprintf(stderr, "airlift decode: %s: expected an object with a 'frames' list of strings\n", *framesPath)
		return 2
	}
	r := beam.Decode(dump.Frames)
	line := fmt.Sprintf("frames   %d: accepted %d, dup %d, bad %d, foreign %d", len(dump.Frames), r.Accepted, r.Dup, r.Bad, r.Foreign)
	if r.Packets > 0 {
		line += fmt.Sprintf(", fountain packets %d", r.Packets)
	}
	fmt.Fprintln(stdout, line)
	if r.HasSession {
		fmt.Fprintf(stdout, "session  0x%08x\n", r.Session)
	}
	if r.Manifest != nil {
		m := r.Manifest
		fmt.Fprintf(stdout, "manifest %s: %d chunks × %d bytes, %d → %d bytes\n", m.Name, m.Total(), m.Chunk, m.GzSize, m.OrigSize)
	}
	if !r.OK() {
		fmt.Fprintf(stderr, "FAILED   %v\n", r.Err)
		if len(r.Missing) > 0 {
			shown := r.Missing
			suffix := ""
			if len(shown) > 10 {
				shown, suffix = shown[:10], " …"
			}
			parts := make([]string, len(shown))
			for i, v := range shown {
				parts[i] = fmt.Sprintf("%d", v)
			}
			fmt.Fprintf(stderr, "missing  %s%s  (%d packets unresolved)\n", strings.Join(parts, ", "), suffix, r.Pending)
		}
		return 1
	}
	if err := os.WriteFile(*out, r.Data, 0o644); err != nil {
		fmt.Fprintf(stderr, "airlift decode: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "gz_sha256    OK %s\n", r.GzSHA256)
	fmt.Fprintf(stdout, "orig_sha256  OK %s\n", r.OrigSHA256)
	fmt.Fprintf(stdout, "wrote    %s (%d bytes)\n", *out, len(r.Data))
	return 0
}
