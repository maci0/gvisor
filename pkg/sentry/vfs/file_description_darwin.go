//go:build darwin

package vfs

import "golang.org/x/sys/unix"

// isErrNoData checks for macOS's ENOATTR (errno 93), which is the
// equivalent of Linux's ENODATA for extended attribute operations.
func isErrNoData(err error) bool {
	return err == unix.Errno(93) // ENOATTR
}
