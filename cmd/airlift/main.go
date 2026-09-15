// Command airlift is the single binary for optical file transfer out of an
// air-gapped machine. Two user-facing subcommands:
//
//	beam    bundle a folder/file(s) into a named beam and open its QR page,
//	        or send it straight to a session from a connected machine (-s)
//	tower   host the airlift server that scanners relay to
//
// Bundling, the frame codec, QR rendering, reassembly and the dev replay all
// live in internal packages; the binary exposes only what a transfer needs.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const usage = `airlift — optical file transfer out of an air-gapped machine.

usage: airlift <command> [flags]

commands:
  beam    bundle a folder or file(s) into a named beam: an offline QR page,
          or with -s LINK straight to a session (not air-gapped)
  tower   host a session, decode relayed frames, verify and serve

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
	case "beam":
		return cmdBeam(rest, stdout, stderr)
	case "tower":
		return cmdTower(rest, stdout, stderr)
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

// readFileList reads a --files-from list: one path per line, dropping blanks
// and # comments. A leading `name: X` line sets the beam name and is returned
// separately.
func readFileList(path string) (paths []string, name string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	for _, ln := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(ln)
		if s == "" || strings.HasPrefix(strings.TrimLeft(ln, " \t"), "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(s, "name:"); ok {
			name = strings.TrimSpace(rest)
			continue
		}
		paths = append(paths, s)
	}
	return paths, name, nil
}

// openBrowser opens path in the platform's default handler. Failure is not
// fatal — the caller reports it and leaves the file in place.
func openBrowser(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name, args = "open", []string{abs}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", abs}
	default:
		name, args = "xdg-open", []string{abs}
	}
	return exec.Command(name, args...).Start()
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
