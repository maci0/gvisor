// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin

package gofer

// This file provides stub implementations of the directfs inode
// subsystem for macOS. Directfs mode is not supported on macOS
// because it requires Linux-specific APIs (O_PATH, /proc/self/fd,
// AT_EMPTY_PATH, etc.). The gofer filesystem on macOS always uses
// the lisafs RPC path instead.

import (
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"golang.org/x/sys/unix"
)

// directfsInode is a stub for macOS. Directfs mode is not supported.
//
// +stateify savable
type directfsInode struct {
	inode

	controlFD     int
	controlFDLisa lisafs.ClientFD `state:"nosave"`
}

func (fs *filesystem) getDirectfsRootDentry(ctx context.Context, rootHostFD int, rootControlFD lisafs.ClientFD) (*dentry, error) {
	panic("directfs mode is not supported on macOS")
}

func (fs *filesystem) newDirectfsDentry(controlFD int) (*dentry, error) {
	panic("directfs mode is not supported on macOS")
}

func (i *directfsInode) openHandle(ctx context.Context, flags uint32, d *dentry) (handle, error) {
	return handle{}, linuxerr.ENOTSUP
}

func (i *directfsInode) ensureLisafsControlFD(ctx context.Context, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) updateMetadataLocked(h handle) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) updateMetadataFromStatLocked(stat *unix.Stat_t) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) chmod(ctx context.Context, mode uint16, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) utimensat(ctx context.Context, stat *linux.Statx, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) prepareSetStat(ctx context.Context, stat *linux.Statx, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) setStatLocked(ctx context.Context, stat *linux.Statx, d *dentry) (uint32, error) {
	return 0, linuxerr.ENOTSUP
}

func (i *directfsInode) destroy(ctx context.Context) {}

func (i *directfsInode) getHostChild(name string) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) getXattr(ctx context.Context, name string, size uint64, d *dentry) (string, error) {
	return "", linuxerr.ENOTSUP
}

func (i *directfsInode) getCreatedChild(name string, uid auth.KUID, gid auth.KGID, isDir bool, createDentry bool, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) mknod(ctx context.Context, name string, creds *auth.Credentials, opts *vfs.MknodOptions, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) bindAt(ctx context.Context, name string, creds *auth.Credentials, opts *vfs.MknodOptions, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) link(target *dentry, name string, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) mkdir(name string, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) symlink(name, target string, creds *auth.Credentials, d *dentry) (*dentry, error) {
	return nil, linuxerr.ENOTSUP
}

func (i *directfsInode) openCreate(name string, accessFlags uint32, mode linux.FileMode, uid auth.KUID, gid auth.KGID, createDentry bool, d *dentry) (*dentry, handle, error) {
	return nil, handle{}, linuxerr.ENOTSUP
}

func (i *directfsInode) getDirentsLocked(recordDirent func(name string, key inoKey, dType uint8), d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) connect(ctx context.Context, sockType linux.SockType, euid lisafs.UID, egid lisafs.GID, d *dentry) (int, error) {
	return -1, linuxerr.ENOTSUP
}

func (i *directfsInode) readlink() (string, error) {
	return "", linuxerr.ENOTSUP
}

func (i *directfsInode) unlink(ctx context.Context, name string, flags uint32, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) rename(ctx context.Context, oldName string, newDir *directfsInode, newName string, d *dentry) error {
	return linuxerr.ENOTSUP
}

func (i *directfsInode) statfs() (linux.Statfs, error) {
	return linux.Statfs{}, linuxerr.ENOTSUP
}

func (i *directfsInode) restoreFile(ctx context.Context, controlFD int, opts *vfs.CompleteRestoreOptions, d *dentry) error {
	return linuxerr.ENOTSUP
}

func doRevalidationDirectfs(ctx context.Context, vfsObj *vfs.VirtualFilesystem, state *revalidateState, ds **[]*dentry) error {
	return linuxerr.ENOTSUP
}

// tryOpen provides darwin-compatible file opening.
func tryOpen(open func(int) (int, error)) (int, error) {
	flags := []int{
		unix.O_RDONLY | unix.O_NONBLOCK,
		// macOS doesn't have O_PATH; try O_SYMLINK for symlinks.
		unix.O_SYMLINK | unix.O_NOFOLLOW,
	}

	var (
		hostFD int
		err    error
	)
	for _, flag := range flags {
		hostFD, err = open(flag | unix.O_NOFOLLOW | unix.O_CLOEXEC)
		if err == nil {
			return hostFD, nil
		}
		if err == unix.ENOENT {
			break
		}
	}
	return -1, err
}

// hostOpenFlags for darwin.
const hostOpenFlags = unix.O_NOFOLLOW | unix.O_CLOEXEC
