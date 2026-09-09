// Command airlift is the single binary for optical file transfer out of an
// air-gapped machine. It packs a folder or files into a repobundle, renders a
// file or a tree as an animated QR loop (the beam), dumps and decodes frames,
// hosts the tower that reassembles and verifies what scanners relay, and
// replays a dump into a session for development. One static binary runs inside
// the air gap; the same binary runs the tower on the operator's laptop.
//
// Subcommands: pack, unpack, beam, frames, decode, tower, replay. See the
// project README and docs/.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

const usage = `airlift — optical file transfer out of an air-gapped machine.

usage: airlift <command> [flags]

commands:
  pack     pack a folder or files into a repobundle text file
  unpack   restore files from a repobundle, checking every sha256
  beam     write a self-contained HTML QR player for a file or a tree
  frames   dump {sender_session, manifest, frames} as JSON
  decode   reassemble a file from a frames dump
  tower    host a session, decode relayed frames, verify and serve
  replay   feed a frames dump into a tower session (dev loop, no camera)

Run "airlift <command> -h" for a command's flags.
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "pack":
		return cmdPack(rest, stdout, stderr)
	case "unpack":
		return cmdUnpack(rest, stdout, stderr)
	case "beam":
		return cmdBeam(rest, stdout, stderr)
	case "frames":
		return cmdFrames(rest, stdout, stderr)
	case "decode":
		return cmdDecode(rest, stdout, stderr)
	case "tower":
		return cmdTower(rest, stdout, stderr)
	case "replay":
		return cmdReplay(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "airlift: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// parsePermuted parses fs, allowing positional arguments to be interspersed
// with flags: the flag package stops at the first non-flag token, so we pull it
// aside and parse the rest, repeating. It returns the positionals in order.
func parsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// writeDump writes a frames dump as indented JSON with a trailing newline.
func writeDump(path string, d any) error {
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// readFileList reads one path per line, dropping blanks and # comments.
func readFileList(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ln := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(strings.TrimLeft(ln, " \t"), "#") {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// human formats a byte count for the beam summary.
func human(n int) string {
	f := float64(n)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if f < 1024 || unit == "GB" {
			if unit == "B" {
				return fmt.Sprintf("%.0f %s", f, unit)
			}
			return fmt.Sprintf("%.1f %s", f, unit)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f GB", f)
}
