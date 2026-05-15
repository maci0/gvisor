# Gofer Filesystem on macOS

[TOC]

The gofer provides host filesystem passthrough on macOS via the lisafs protocol.
An in-process lisafs server runs in a goroutine connected to the sentry via a
Unix socketpair.

## Architecture

```
 sentrydarwin process
 +-------------------------------------------------------+
 |                                                       |
 |  Sentry VFS                     In-process Gofer      |
 |  +-------------------+         +-------------------+  |
 |  | gofer.Filesystem  |         | fsgofer.LisafsServer |
 |  | (lisafs client)   | <-----> | (lisafs server)      |
 |  |                   | socket  |                      |
 |  | Mounted at /      | pair    | Reads/writes host    |
 |  +-------------------+         | filesystem directly  |
 |                                +-------------------+  |
 |                                         |              |
 |                                    Host filesystem     |
 |                                    (--rootfs dir)      |
 +-------------------------------------------------------+
```

Unlike Linux gVisor where the gofer runs as a separate process for security
isolation, the macOS port runs the gofer in-process. This simplifies deployment
while maintaining the same lisafs protocol interface.

## Linux API Porting

The fsgofer (`runsc/fsgofer/lisafs.go`) was ported to macOS by extracting all
Linux-specific syscalls into platform-split files:

- `runsc/fsgofer/lisafs_compat_linux.go` - Linux implementations
- `runsc/fsgofer/lisafs_compat_darwin.go` - macOS implementations

### /proc/self/fd (FD Reopening)

Linux uses `/proc/self/fd/N` to reopen file descriptors with different flags.
macOS uses `fcntl(F_GETPATH)` to get the file path, then reopens via `open()`.
Falls back to `dup()` when the path-based reopen fails (e.g., directories
opened via `/dev/fd/N` return "not a directory").

### O_PATH

Linux's `O_PATH` opens a file descriptor for metadata operations without
actually reading the file. macOS has no equivalent. The port uses `O_RDONLY`
instead, which works for most gofer operations. For symlinks, where `O_RDONLY`
returns `ELOOP` on macOS, the gofer falls back to `fstatat` with
`AT_SYMLINK_NOFOLLOW`.

### O_LARGEFILE (0x20000)

Linux's `O_LARGEFILE` flag (value 0x20000) is harmless on Linux but maps to
`O_EVTONLY` on macOS, which makes file descriptors event-notification-only
(unreadable). All open flags are masked through `darwinOpenMask` to strip
Linux-only flags before passing to macOS `open()`.

### Directory Listing

Linux uses `getdents()` / `getdents64()`. macOS uses `getdirentries()` with a
different dirent struct layout. The macOS `Dirent` struct has a 1024-byte `Name`
field (`sizeof(Dirent)` = 1048), but actual entries from `getdirentries()` are
~32 bytes each. The `ParseDirents` function was fixed to check against the
minimum header size (21 bytes) instead of the full struct size.

### Symlink Handling

```
 Linux:     open(name, O_PATH | O_NOFOLLOW)  ->  FD to symlink itself
 macOS:     open(name, O_RDONLY | O_NOFOLLOW) ->  ELOOP (can't open symlinks)
```

The gofer handles ELOOP in Walk() and WalkStat():

1. Detect `ELOOP` from `openat()` with `O_NOFOLLOW`
2. Call `unix.Fstatat(dirfd, name, &stat, AT_SYMLINK_NOFOLLOW)` to get stat
3. Return symlink stat to sentry; sentry resolves via `Readlink` + re-walk
4. `Readlink()` falls back to `readlink(node.FilePath())` since the control
   FD is a dup of the parent directory

Note: must use the Go `unix.Fstatat` wrapper, not the raw `SYS_FSTATAT`
syscall, which returns incorrect `Stat_t` fields on macOS ARM64.

## flipcall Channels

The lisafs protocol uses flipcall channels for parallel RPCs. On macOS:

- `SOCK_SEQPACKET` -> `SOCK_STREAM` (SEQPACKET not supported for AF_UNIX)
- Zero-length SCM_RIGHTS messages -> 1-byte dummy data (SOCK_STREAM requires
  at least 1 data byte for control message delivery)
- `F_ADD_SEALS` on memfd -> no-op (macOS shm_open doesn't support seals)
- `SOCK_CLOEXEC` flag -> `fcntl(F_SETFD, FD_CLOEXEC)` after socket creation

## Sentry-Side Gofer Package

The sentry-side gofer (`pkg/sentry/fsimpl/gofer/`) has a `directfs` mode that
bypasses lisafs for direct host syscalls. This mode requires `O_PATH`,
`/proc/self/fd`, `AT_EMPTY_PATH`, `dup3`, `statx`, `fallocate`, and `tee` -
none of which are available on macOS.

On macOS, `directfs_inode.go` is excluded via `//go:build linux` and replaced
by `directfs_inode_stubs_darwin.go` with panic/ENOTSUP stubs. All filesystem
operations go through the lisafs RPC path, which is fully portable.
