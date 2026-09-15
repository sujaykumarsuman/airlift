package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sujaykumarsuman/airlift/internal/term"
)

// status is the one live line a long step draws. On a terminal it is redrawn
// in place and wiped before anything permanent is printed; anywhere else (a
// pipe, a log, a test) only a changed milestone is written, once, as a plain
// line — so a script's output stays short and readable.
type status struct {
	w         io.Writer
	tty       bool
	cols      func() int // the terminal's width, read per draw (it can be resized)
	shown     bool
	milestone string
	lastDraw  time.Time
}

func newStatus(w io.Writer) *status {
	f, ok := w.(*os.File)
	st := &status{w: w, tty: ok && term.IsTerminal(f)}
	if st.tty {
		st.cols = func() int { return term.Width(f) }
	}
	return st
}

// fit cuts line to one terminal row: `\r\033[K` clears only the row the cursor
// is on, so a wrapped live line would leave its first row behind on every
// redraw.
func (s *status) fit(line string) string {
	cols := 80
	if s.cols != nil {
		if c := s.cols(); c > 0 {
			cols = c
		}
	}
	if r := []rune(line); len(r) > cols-1 {
		return string(r[:max(cols-2, 1)]) + "…"
	}
	return line
}

// set shows line. milestone names the step (and, for a bar, its quarter):
// off a terminal a line is written only when the milestone changes. On a
// terminal redraws are held to ten a second.
func (s *status) set(milestone, line string) {
	if !s.tty {
		if milestone != s.milestone {
			s.milestone = milestone
			fmt.Fprintln(s.w, line)
		}
		return
	}
	now := time.Now()
	if s.shown && milestone == s.milestone && now.Sub(s.lastDraw) < 100*time.Millisecond {
		return
	}
	s.milestone, s.lastDraw, s.shown = milestone, now, true
	fmt.Fprintf(s.w, "\r\033[K%s", s.fit(line))
}

// live draws line on a terminal only (throttled like set): a detail worth
// watching as it happens and not worth a line in a log.
func (s *status) live(line string) {
	if s.tty {
		s.set("live", line)
	}
}

// clear wipes the live line (a terminal only) so permanent output starts clean.
func (s *status) clear() {
	if s.tty && s.shown {
		fmt.Fprint(s.w, "\r\033[K")
		s.shown = false
	}
	s.milestone = ""
}

// bar is a fixed-width progress bar for done out of total.
func bar(done, total int64, width int) string {
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := int(float64(width) * float64(min(done, total)) / float64(total))
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// quarter is the milestone bucket for a percentage: 0, 25, 50, 75 or 100.
func quarter(done, total int64) string {
	if total <= 0 {
		return "0"
	}
	return fmt.Sprint(25 * (100 * min(done, total) / total / 25))
}

// rate formats bytes per second.
func rate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return humanBytes(int64(float64(bytes)/d.Seconds())) + "/s"
}

// clock formats a remaining duration as m:ss.
func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// elapsed formats a duration for a summary: milliseconds under a second.
func elapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1f s", d.Seconds())
}

// humanBytes is human for an int64.
func humanBytes(n int64) string { return human(int(n)) }

// countingWriter reports how many bytes have passed through it.
type countingWriter struct {
	w      io.Writer
	n      int64
	report func(n int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.report != nil {
		c.report(c.n)
	}
	return n, err
}
