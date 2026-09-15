//go:build darwin || freebsd || netbsd || openbsd

package term

import "syscall"

const (
	ioctlGet = syscall.TIOCGETA
	ioctlSet = syscall.TIOCSETA
)
