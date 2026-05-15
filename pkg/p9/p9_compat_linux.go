//go:build linux
// +build linux

package p9

import (
	"syscall"

	"golang.org/x/sys/unix"
)

func statAtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Atim.Sec), uint64(s.Atim.Nsec)
}

func statMtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Mtim.Sec), uint64(s.Mtim.Nsec)
}

func statCtime(s *syscall.Stat_t) (sec uint64, nsec uint64) {
	return uint64(s.Ctim.Sec), uint64(s.Ctim.Nsec)
}

const (
	fallocKeepSize      = unix.FALLOC_FL_KEEP_SIZE
	fallocPunchHole     = unix.FALLOC_FL_PUNCH_HOLE
	fallocNoHideStale   = unix.FALLOC_FL_NO_HIDE_STALE
	fallocCollapseRange = unix.FALLOC_FL_COLLAPSE_RANGE
	fallocZeroRange     = unix.FALLOC_FL_ZERO_RANGE
	fallocInsertRange   = unix.FALLOC_FL_INSERT_RANGE
	fallocUnshareRange  = unix.FALLOC_FL_UNSHARE_RANGE
)
