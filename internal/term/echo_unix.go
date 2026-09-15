//go:build linux || darwin || freebsd || netbsd || openbsd

package term

import (
	"os"
	"syscall"
	"unsafe"
)

func getTermios(f *os.File) (syscall.Termios, bool) {
	var t syscall.Termios
	_, _, e := syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), ioctlGet, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return t, e == 0
}

func isTerminal(f *os.File) bool {
	_, ok := getTermios(f)
	return ok
}

func width(f *os.File) int {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)), 0, 0, 0); e != 0 {
		return 0
	}
	return int(ws.Col)
}

// echoOff clears ECHO on f's terminal (canonical input and signals stay on, as
// a password read wants) and returns the restorer.
func echoOff(f *os.File) (func(), error) {
	old, ok := getTermios(f)
	if !ok {
		return nil, syscall.ENOTTY
	}
	quiet := old
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	quiet.Iflag |= syscall.ICRNL
	if _, _, e := syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), ioctlSet, uintptr(unsafe.Pointer(&quiet)), 0, 0, 0); e != 0 {
		return nil, e
	}
	return func() {
		syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), ioctlSet, uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}, nil
}
