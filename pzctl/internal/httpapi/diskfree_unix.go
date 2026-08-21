//go:build unix

package httpapi

import "syscall"

// diskFree is the space available to an unprivileged writer on the filesystem
// holding dir.
//
// Bavail, not Bfree: the difference is the reserved blocks only root may use, and
// the controller does not run as root. Reporting Bfree would let an upload pass
// the check and then fail with ENOSPC, which is the exact failure the check
// exists to convert into a clean 507.
func diskFree(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
