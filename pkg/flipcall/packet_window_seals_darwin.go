//go:build darwin

package flipcall

// applyMemfdSeals is a no-op on macOS. macOS's shm_open-based memfd
// does not support F_ADD_SEALS.
func applyMemfdSeals(fd int) error {
	return nil
}
