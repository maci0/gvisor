//go:build linux

package vfs

// isErrNoData is always false on Linux since ENODATA is already
// handled by the linuxerr.Equals check.
func isErrNoData(_ error) bool {
	return false
}
