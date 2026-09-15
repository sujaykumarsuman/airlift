// Package term is the little the CLI needs from a terminal: whether a file is
// one, how wide it is, and turning echo off while a password is typed. It stays
// inside the standard library (the project's dependency rule) by asking the
// console directly — termios and window-size ioctls on Unix, the console mode
// and screen buffer on Windows.
package term

import "os"

// IsTerminal reports whether f is an interactive terminal. A character device
// is not enough — /dev/null is one — so it asks the terminal driver.
func IsTerminal(f *os.File) bool { return isTerminal(f) }

// Width is f's terminal width in columns, or 0 when it cannot be read.
func Width(f *os.File) int { return width(f) }

// EchoOff stops f's terminal echoing what is typed and returns the function
// that restores it. It fails where the console cannot be controlled; a caller
// then reads visibly rather than not at all.
func EchoOff(f *os.File) (restore func(), err error) { return echoOff(f) }
