package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sujaykumarsuman/airlift/internal/term"
)

// prompter asks for what a run needs but was not given — only when both stdin
// and stderr (where the question is printed) are a terminal, so a script, a
// pipe or a redirected log gets a plain error naming the flag instead of a
// silent hang.
type prompter struct {
	in          *bufio.Reader
	out         io.Writer
	interactive bool
	echoOff     func() (restore func(), err error)
	ctx         context.Context // an interrupt ends a question (a read cannot watch signals)
}

// newPrompter builds the run's prompter over stdin; tests replace it.
var newPrompter = func(stderr io.Writer) *prompter {
	errFile, _ := stderr.(*os.File)
	return &prompter{
		in:          bufio.NewReader(os.Stdin),
		out:         stderr,
		interactive: term.IsTerminal(os.Stdin) && errFile != nil && term.IsTerminal(errFile),
		echoOff:     func() (func(), error) { return term.EchoOff(os.Stdin) },
		ctx:         context.Background(),
	}
}

// errNotInteractive is what a prompt returns off a terminal; callers wrap it
// with the flag that would have answered.
var errNotInteractive = errors.New("not a terminal")

// ask prints "  label    question: " and returns the trimmed answer.
func (p *prompter) ask(label, question string) (string, error) {
	if !p.interactive {
		return "", errNotInteractive
	}
	fmt.Fprintf(p.out, "  %-8s %s: ", label, question)
	line, err := p.readLine()
	if err != nil {
		fmt.Fprintln(p.out)
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// askSecret is ask with the terminal's echo off (a password); echo comes back
// however the question ends, an interrupt included.
func (p *prompter) askSecret(label, question string) (string, error) {
	if !p.interactive {
		return "", errNotInteractive
	}
	fmt.Fprintf(p.out, "  %-8s %s: ", label, question)
	if restore, err := p.echoOff(); err == nil {
		defer func() {
			restore()
			fmt.Fprintln(p.out) // the Enter the user typed was not echoed
		}()
	}
	line, err := p.readLine()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readLine reads one line, giving up when the run is interrupted. The read
// itself cannot be cancelled, so it runs aside; an interrupted run is ending.
func (p *prompter) readLine() (string, error) {
	type result struct {
		line string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := p.in.ReadString('\n')
		got <- result{line, err}
	}()
	ctx := p.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case r := <-got:
		if r.err != nil && !(errors.Is(r.err, io.EOF) && r.line != "") {
			return "", fmt.Errorf("no answer: %w", r.err)
		}
		return r.line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// splitPaths splits a typed or pasted line of paths the way a shell would for
// the common cases: whitespace separates, single and double quotes group, a
// backslash escapes the next character (a path dragged into a macOS terminal
// arrives as `/Users/me/My\ Files`; on Windows the backslash is the separator
// and stays literal). A leading ~/ is the home directory.
func splitPaths(line string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord, quote, escaped := false, rune(0), false
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\\' && filepath.Separator != '\\':
			escaped, inWord = true, true
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, expandHome(cur.String()))
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unmatched quote")
	}
	if inWord {
		out = append(out, expandHome(cur.String()))
	}
	return out, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
