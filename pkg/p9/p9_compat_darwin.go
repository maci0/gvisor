//go:build darwin
// +build darwin

package p9

import "syscall"

func statAtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Atimespec.Sec), uint64(s.Atimespec.Nsec)
}

func statMtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Mtimespec.Sec), uint64(s.Mtimespec.Nsec)
}

func statCtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Ctimespec.Sec), uint64(s.Ctimespec.Nsec)
}

// fallocate(2) constants - Linux-specific, defined here for compilation.
// These values match the Linux definitions.
const (
	fallocKeepSize      = 0x01
	fallocPunchHole     = 0x02
	fallocNoHideStale   = 0x04
	fallocCollapseRange = 0x08
	fallocZeroRange     = 0x10
	fallocInsertRange   = 0x20
	fallocUnshareRange  = 0x40
)
