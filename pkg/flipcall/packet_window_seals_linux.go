//go:build linux

package flipcall

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func applyMemfdSeals(fd int) error {
	if _, _, e := unix.RawSyscall(unix.SYS_FCNTL, uintptr(fd), linux.F_ADD_SEALS, linux.F_SEAL_SHRINK|linux.F_SEAL_SEAL); e != 0 {
		return fmt.Errorf("failed to apply memfd seals: %v", e)
	}
	return nil
}
