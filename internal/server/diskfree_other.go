//go:build !linux && !darwin && !freebsd && !windows

package server

// diskFree cannot read free space here; the upload checks are skipped and a
// full disk fails the write instead.
func diskFree(string) (int64, bool) { return 0, false }
