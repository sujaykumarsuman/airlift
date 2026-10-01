//go:build linux || darwin || freebsd

package server

import "syscall"

// diskFree is the space an unprivileged process can still write on the
// filesystem holding dir; ok is false when it cannot be read.
func diskFree(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize)), true
}
