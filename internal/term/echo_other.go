//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package term

import (
	"errors"
	"os"
)

// Without a terminal driver to ask, a character device is the best guess; the
// width is unknown and echo cannot be turned off (a password is read visibly).
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func width(*os.File) int { return 0 }

func echoOff(*os.File) (func(), error) { return nil, errors.New("no terminal control") }
