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

//go:build darwin && arm64

// Command sentry-darwin runs Linux ARM64 ELF binaries on macOS using the
// gVisor sentry kernel with Hypervisor.framework (HVF) as the platform.
//
// Usage:
//
//	sentry-darwin [flags] <static-linux-elf> [args...]
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/devices/memdev"
	"gvisor.dev/gvisor/pkg/sentry/devices/ttydev"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/dev"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/devpts"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/devtmpfs"
	goferfs "gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/host"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/proc"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/loader"
	"gvisor.dev/gvisor/pkg/sentry/mm"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf"
	_ "gvisor.dev/gvisor/pkg/sentry/platform/platforms"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack" // also registers AF_INET provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink"        // AF_NETLINK provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/route"  // NETLINK_ROUTE provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/uevent" // NETLINK_KOBJECT_UEVENT
	_ "gvisor.dev/gvisor/pkg/sentry/socket/unix"           // AF_UNIX provider
	"gvisor.dev/gvisor/pkg/sentry/strace"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux" // register syscall table
	"gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/link/proxynet"
	"gvisor.dev/gvisor/pkg/tcpip/link/utun"
	"gvisor.dev/gvisor/pkg/tcpip/link/vmnet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/waiter"
	"gvisor.dev/gvisor/runsc/fsgofer"
)

var (
	flagStrace     = flag.Bool("strace", false, "enable system call tracing")
	flagDebug      = flag.Bool("debug", false, "enable debug logging")
	flagLog        = flag.String("log", "", "file path for log output (default: stderr)")
	flagRootfs     = flag.String("rootfs", "", "host directory to use as guest root filesystem (via gofer)")
	flagNet        = flag.String("net", "", "networking mode: proxy (default), utun (root), vmnet (rootless daemon)")
	flagNetwork    = flag.String("network", "", "alias for --net (runsc compat)")
	flagVmnetSocket = flag.String("vmnet-socket", "", "socket_vmnet Unix socket path (default: auto-detect)")
	flagGuestIP    = flag.String("guest-ip", "192.168.105.100", "guest IP address for vmnet mode")
	flagCPUs       = flag.Int("cpus", 0, "number of vCPUs (0 = auto-detect)")
	flagKeepRoot   = flag.Bool("keep-root", false, "don't drop root privileges after network setup")
	flagRootless   = flag.Bool("rootless", false, "run without root (no utun, no privilege drop)")
	flagPlatform   = flag.String("platform", "hvf", "platform backend: hvf (default)")
	flagDirectfs   = flag.Bool("directfs", false, "enable directfs mode (bypass lisafs RPC)")
	flagPage4K     = flag.Bool("page4k", true, "use 4K guest pages (default, matching Linux ARM64)")
	flagPage16K    = flag.Bool("page16k", false, "use 16K guest pages (macOS native)")
	flagProfile    = flag.String("profile", "", "write per-Switch() timing stats to file")
	flagMachMemory = flag.Bool("mach-memory", false, "use Mach anonymous memory for MemoryFile")
)

func main() {
	// Rewrite bare "--net" (no value) to "--net=proxy" (zero-dependency default).
	for i, arg := range os.Args {
		if arg == "--net" || arg == "-net" {
			if i+1 >= len(os.Args) || strings.HasPrefix(os.Args[i+1], "-") {
				os.Args[i] = arg + "=proxy"
			}
		}
	}
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <linux-elf> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	// --network is an alias for --net (runsc compat)
	if *flagNetwork != "" && *flagNet == "" {
		*flagNet = *flagNetwork
	}

	// --rootless implies no utun (needs root) and no privilege drop
	if *flagRootless {
		*flagKeepRoot = true
	}

	// Logging
	if *flagDebug {
		log.SetLevel(log.Debug)
	} else {
		log.SetLevel(log.Warning)
	}
	if *flagLog != "" {
		f, err := os.Create(*flagLog)
		if err != nil {
			fatal("open log: %v", err)
		}
		log.SetTarget(&log.Writer{Next: f})
	}

	// Enable Mach anonymous memory if requested.
	if *flagMachMemory {
		pgalloc.SetUseMachMemory(true)
		log.Infof("Using Mach anonymous memory for MemoryFile (experimental)")
		fmt.Fprintf(os.Stderr, "[mach-memory] Enabled: MemoryFile backed by mach_make_memory_entry_64\n")
	}

	// 4K guest pages (default). Linux ARM64 universally uses 4K pages.
	// --page16k overrides to macOS-native 16K if needed.
	if *flagPage4K && !*flagPage16K {
		loader.SetPage4KMode(true)
		mm.SetPage4KMode(true)
		log.Infof("4K guest pages enabled (AT_PAGESZ=4096)")
	}

	// Configure ARM64 address space for VA48 (256TB).
	arch.ConfigureAddressSpace(1 << 48)

	elfPath := flag.Arg(0)
	guestArgs := flag.Args()

	// Initialize memory usage tracking.
	if err := usage.Init(); err != nil {
		fatal("usage.Init: %v", err)
	}

	if *flagPlatform != "hvf" {
		fatal("unsupported platform %q (only hvf is available)", *flagPlatform)
	}
	plat, err := hvf.New(hvf.HVFOpts{
		Page4K: *flagPage4K && !*flagPage16K,
	})
	if err != nil {
		fatal("platform create: %v", err)
	}

	// Create the kernel.
	k := &kernel.Kernel{
		Platform: plat,
	}

	// Create memory file (backing store for guest memory).
	memFD, err := memutil.CreateMemFD("sentry-memory", 0)
	if err != nil {
		fatal("CreateMemFD: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memFD), "sentry-memory"), pgalloc.MemoryFileOpts{})
	if err != nil {
		fatal("NewMemoryFile: %v", err)
	}
	k.SetMemoryFile(mf)

	// Prepare VDSO.
	v, err := loader.PrepareVDSO(mf)
	if err != nil {
		fatal("PrepareVDSO: %v", err)
	}

	// Create timekeeper.
	tk := kernel.NewTimekeeper()
	vdsoParams := kernel.NewVDSOParamPage(mf, v.ParamPage.FileRange())
	tk.SetClocks(time.NewCalibratedClocks(), vdsoParams)

	// Root credentials and namespaces.
	creds := auth.NewRootCredentials(auth.NewRootUserNamespace())

	numCPU := runtime.NumCPU()
	if *flagCPUs > 0 {
		numCPU = *flagCPUs
	}
	if numCPU > 64 {
		numCPU = 64
	}
	// Each vCPU goroutine calls LockOSThread, consuming one OS thread
	// permanently. Ensure GOMAXPROCS is large enough for vCPUs plus
	// the in-process gofer, timers, and other goroutines.
	minProcs := numCPU + 8
	if current := runtime.GOMAXPROCS(0); current < minProcs {
		runtime.GOMAXPROCS(minProcs)
	}

	// --- Network stack ---
	ns := createNetworkStack(tk, k, *flagNet)
	netns := inet.NewRootNamespace(ns, nil, creds.UserNamespace)

	cpuid.Initialize()
	if err := k.Init(kernel.InitKernelArgs{
		FeatureSet:           cpuid.HostFeatureSet().Fixed(),
		Timekeeper:           tk,
		RootUserNamespace:    creds.UserNamespace,
		ApplicationCores:     uint(numCPU),
		Vdso:                 v,
		VdsoParams:           vdsoParams,
		RootUTSNamespace:     kernel.NewUTSNamespace("gvisor-darwin", "gvisor-darwin", creds.UserNamespace),
		RootIPCNamespace:     kernel.NewIPCNamespace(creds.UserNamespace),
		RootPIDNamespace:     kernel.NewRootPIDNamespace(creds.UserNamespace),
		RootNetworkNamespace: netns,
	}); err != nil {
		fatal("kernel.Init: %v", err)
	}

	// Enable system call tracing if requested.
	if *flagStrace {
		strace.Initialize()
		strace.EnableAll(strace.SinkTypeLog)
		log.SetLevel(log.Debug)
	}

	// Register filesystems.
	vfsObj := k.VFS()
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, &tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})
	vfsObj.MustRegisterFilesystemType(proc.Name, &proc.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})
	vfsObj.MustRegisterFilesystemType(dev.Name, &dev.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{})
	vfsObj.MustRegisterFilesystemType(devtmpfs.Name, &devtmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})
	vfsObj.MustRegisterFilesystemType(devpts.Name, &devpts.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})

	// Register devices.
	if err := memdev.Register(vfsObj); err != nil {
		fatal("registering memdev: %v", err)
	}
	if err := ttydev.Register(vfsObj); err != nil {
		fatal("registering ttydev: %v", err)
	}

	// Set up host filesystem for importing host FDs.
	hostFS, err := host.NewFilesystem(vfsObj)
	if err != nil {
		fatal("host.NewFilesystem: %v", err)
	}
	defer hostFS.DecRef(k.SupervisorContext())
	k.SetHostMount(vfsObj.NewDisconnectedMount(hostFS, nil, &vfs.MountOptions{}))

	// Create root filesystem. When --rootfs is set, use a gofer filesystem
	// backed by an in-process lisafs server for host directory passthrough.
	// Otherwise, use tmpfs.
	var mntns *vfs.MountNamespace
	if *flagRootfs != "" {
		absRootfs, err := filepath.Abs(*flagRootfs)
		if err != nil {
			fatal("abs rootfs path: %v", err)
		}
		// Ensure mount-point directories exist on the host rootfs.
		// The sentry will mount procfs, devtmpfs, devpts, and tmpfs
		// on top of these. Without them, /dev/null, /dev/fd, /tmp,
		// and /proc are unavailable to the guest.
		for _, dir := range []string{"proc", "dev", "dev/pts", "tmp", "sys"} {
			os.MkdirAll(filepath.Join(absRootfs, dir), 0755)
		}
		mntns = setupGoferRoot(k, vfsObj, creds, absRootfs)
	} else {
		var err error
		mntns, err = vfsObj.NewMountNamespace(
			k.SupervisorContext(),
			creds,
			"none",
			tmpfs.Name,
			&vfs.MountOptions{},
			nil,
		)
		if err != nil {
			fatal("NewMountNamespace: %v", err)
		}
	}

	// Create mount point directories and mount filesystems.
	root := mntns.Root(k.SupervisorContext())
	defer root.DecRef(k.SupervisorContext())

	for _, dir := range []string{"/proc", "/dev", "/dev/pts", "/tmp"} {
		pop := vfs.PathOperation{
			Root:  root,
			Start: root,
			Path:  fspath.Parse(dir),
		}
		if err := vfsObj.MkdirAt(k.SupervisorContext(), creds, &pop, &vfs.MkdirOptions{
			Mode: 0755,
		}); err != nil {
			log.Debugf("mkdir %s: %v", dir, err)
		}
	}

	// Mount procfs at /proc.
	procPop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse("/proc"),
	}
	if _, err := vfsObj.MountAt(k.SupervisorContext(), creds, "none", &procPop, proc.Name, &vfs.MountOptions{}); err != nil {
		log.Warningf("mount /proc: %v", err)
	}

	// Mount dev at /dev.
	devPop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse("/dev"),
	}
	if _, err := vfsObj.MountAt(k.SupervisorContext(), creds, "none", &devPop, dev.Name, &vfs.MountOptions{
		GetFilesystemOptions: vfs.GetFilesystemOptions{InternalMount: true},
	}); err != nil {
		log.Warningf("mount /dev: %v", err)
	}

	// Mount devpts at /dev/pts.
	ptsPop := vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse("/dev/pts"),
	}
	if _, err := vfsObj.MountAt(k.SupervisorContext(), creds, "none", &ptsPop, devpts.Name, &vfs.MountOptions{}); err != nil {
		log.Warningf("mount /dev/pts: %v", err)
	}

	// Mount tmpfs on /tmp so guest writes don't pollute the host rootfs.
	if *flagRootfs != "" {
		tmpPop := vfs.PathOperation{
			Root:  root,
			Start: root,
			Path:  fspath.Parse("/tmp"),
		}
		if _, err := vfsObj.MountAt(k.SupervisorContext(), creds, "none", &tmpPop, tmpfs.Name, &vfs.MountOptions{}); err != nil {
			log.Warningf("mount /tmp: %v", err)
		}
	}

	// Detect command mode (-c flag) before setting up stdio.
	hasCmd := false
	for _, a := range guestArgs {
		if a == "-c" {
			hasCmd = true
			break
		}
	}

	// Create FD table with stdin/stdout/stderr.
	ctx := k.SupervisorContext()
	fdTable := k.NewFDTable()

	var stdinTTY *kernel.TTY
	var termState *unix.Termios
	var ptyMasterFD *vfs.FileDescription
	_, isTTYErr := unix.IoctlGetTermios(0, unix.TIOCGETA)
	stdinIsTerminal := isTTYErr == nil

	if stdinIsTerminal && !hasCmd {
		// Interactive mode: set host to raw so keystrokes pass through
		// immediately. The PTY line discipline handles echo/signals.
		termState, _ = unix.IoctlGetTermios(0, unix.TIOCGETA)
		if termState != nil {
			raw := *termState
			raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
			raw.Oflag &^= unix.OPOST
			raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN | unix.ISIG
			raw.Cflag &^= unix.CSIZE | unix.PARENB
			raw.Cflag |= unix.CS8
			raw.Cc[unix.VMIN] = 1
			raw.Cc[unix.VTIME] = 0
			unix.IoctlSetTermios(0, unix.TIOCSETA, &raw)
			// Restore terminal on any exit (panic, signal, normal).
			defer unix.IoctlSetTermios(0, unix.TIOCSETA, termState)
		}

		masterFD, slaveFD, tty, err := openPTY(k, vfsObj, root, creds)
		if err == nil {
			stdinTTY = tty
			ptyMasterFD = masterFD

			if ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ); err == nil {
				devpts.SetWindowSize(masterFD, ws.Row, ws.Col)
			}

			for fd := int32(0); fd < 3; fd++ {
				slaveFD.IncRef()
				if _, err := fdTable.NewFDAt(ctx, fd, slaveFD, kernel.FDFlags{}); err != nil {
					fatal("NewFDAt %d: %v", fd, err)
				}
			}
			slaveFD.DecRef(ctx)

			go pumpPTY(k, masterFD)
		} else {
			log.Warningf("PTY setup failed: %v, falling back to host FDs", err)
		}
	}

	if ptyMasterFD == nil {
		// Non-interactive or PTY fallback: use host FDs directly.
		var stdinFile *vfs.FileDescription
		for hostFD := 0; hostFD < 3; hostFD++ {
			newFD, err := unix.Dup(hostFD)
			if err != nil {
				fatal("dup fd %d: %v", hostFD, err)
			}
			isTTY := stdinIsTerminal

			var f *vfs.FileDescription
			if isTTY && stdinFile != nil && hostFD > 0 {
				f = stdinFile
				f.IncRef()
			} else {
				f, err = host.NewFD(ctx, k.HostMount(), newFD, &host.NewFDOptions{
					IsTTY: isTTY,
				})
				if err != nil {
					unix.Close(newFD)
					fatal("host.NewFD for stdio %d: %v", hostFD, err)
				}
				if isTTY && hostFD == 0 {
					stdinFile = f
					if ttyFD, ok := f.Impl().(*host.TTYFileDescription); ok {
						stdinTTY = ttyFD.TTY()
					}
				}
			}
			if _, err := fdTable.NewFDAt(ctx, int32(hostFD), f, kernel.FDFlags{}); err != nil {
				f.DecRef(ctx)
				fatal("NewFDAt %d: %v", hostFD, err)
			}
			f.DecRef(ctx)
		}
	}

	// Create the initial process. If --rootfs is set, resolve the
	// binary from the guest VFS. Otherwise, open it from the host.
	var elfFile *vfs.FileDescription
	guestFilename := ""
	if *flagRootfs != "" {
		// Resolve from guest VFS (the binary was copied into tmpfs).
		guestFilename = elfPath
	} else {
		// Open directly from host filesystem.
		elfFD, err := unix.Open(elfPath, unix.O_RDONLY, 0)
		if err != nil {
			fatal("open %s: %v", elfPath, err)
		}
		elfFile, err = host.NewFD(k.SupervisorContext(), k.HostMount(), elfFD, &host.NewFDOptions{
			Readonly: true,
		})
		if err != nil {
			fatal("host.NewFD: %v", err)
		}
	}

	mntns.IncRef()
	ls := limits.NewLimitSet()
	tg, _, err := k.CreateProcess(kernel.CreateProcessArgs{
		Filename:             guestFilename,
		File:                 elfFile,
		Argv:                 guestArgs,
		Envv:                 guestEnv(),
		Credentials:          creds,
		FDTable:              fdTable,
		TTY:                  stdinTTY,
		Umask:                0022,
		Limits:               ls,
		MaxSymlinkTraversals: 6,
		UTSNamespace:         k.RootUTSNamespace(),
		IPCNamespace:         k.RootIPCNamespace(),
		PIDNamespace:         k.RootPIDNamespace(),
		MountNamespace:       mntns,
		ContainerID:          "default",
	})
	fdTable.DecRef(ctx)
	if err != nil {
		fatal("CreateProcess: %v", err)
	}

	// Set the sigreturn trampoline address.
	if leader := tg.Leader(); leader != nil {
		if mm := leader.MemoryManager(); mm != nil {
			mm.SetVDSOSigReturn(hvf.SigreturnAddr)
		}
		// Set fast-path syscall values for in-VM dispatch.
		// getpid/gettid/getuid/geteuid handled at EL1 via ERET.
		pid := uint16(k.RootPIDNamespace().IDOfThreadGroup(tg))
		tid := uint16(k.RootPIDNamespace().IDOfTask(leader))
		uid := uint16(creds.RealKUID.In(creds.UserNamespace).OrOverflow())
		euid := uint16(creds.EffectiveKUID.In(creds.UserNamespace).OrOverflow())
		gid := uint16(creds.RealKGID.In(creds.UserNamespace).OrOverflow())
		egid := uint16(creds.EffectiveKGID.In(creds.UserNamespace).OrOverflow())
		// PGID and SID default to PID for the init process
		hvf.PatchInitFastPath(plat, pid, 0, tid, uid, euid, gid, egid, pid, pid)
	}

	log.Infof("Starting gVisor sentry kernel on macOS (HVF platform, %d CPUs)", numCPU)

	// Start the kernel.
	if err := k.Start(); err != nil {
		fatal("kernel.Start: %v", err)
	}

	// Forward host signals to the guest.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGWINCH, unix.SIGALRM)
	go func() {
		for sig := range sigCh {
			s := sig.(unix.Signal)
			switch s {
			case unix.SIGINT:
				if stdinTTY != nil {
					stdinTTY.SignalForegroundProcessGroup(&linux.SignalInfo{
						Signo: int32(linux.SIGINT),
					})
				} else {
					k.SendExternalSignal(&linux.SignalInfo{Signo: int32(s)}, "host")
					k.TaskSet().Kill(linux.WaitStatusTerminationSignal(linux.Signal(s)))
				}
			case unix.SIGWINCH:
				if ptyMasterFD != nil {
					if ws, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ); err == nil {
						devpts.SetWindowSize(ptyMasterFD, ws.Row, ws.Col)
					}
				}
			default:
				k.SendExternalSignal(&linux.SignalInfo{Signo: int32(s)}, "host")
				k.TaskSet().Kill(linux.WaitStatusTerminationSignal(linux.Signal(s)))
			}
		}
	}()

	tg.WaitExited()

	// Drain any remaining output from the PTY master before restoring
	// the terminal. The pump goroutine may not have flushed everything
	// because replicaClose doesn't wake the master waiter.
	if ptyMasterFD != nil {
		buf := make([]byte, 4096)
		for {
			dst := usermem.BytesIOSequence(buf)
			n, err := ptyMasterFD.Read(ctx, dst, vfs.ReadOptions{})
			if n > 0 {
				unix.Write(1, buf[:n])
			}
			if err != nil {
				break
			}
		}
	}

	if ptyMasterFD != nil {
		ptyMasterFD.DecRef(ctx)
	}

	exitStatus := tg.ExitStatus()
	log.Infof("Guest exited with status %d", exitStatus.ExitStatus())
	unix.Fsync(1)
	unix.Fsync(2)
	if *flagProfile != "" {
		hvf.DumpStats(*flagProfile)
	}
	if termState != nil {
		unix.IoctlSetTermios(0, unix.TIOCSETA, termState)
	}
	os.Exit(int(exitStatus.ExitStatus()))
}

// createNetworkStack creates a gVisor netstack with a loopback interface
// and optionally a utun interface for host networking.
func createNetworkStack(clock tcpip.Clock, k *kernel.Kernel, netMode string) *netstack.Stack {
	s := netstack.NewStack(stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4, icmp.NewProtocol6},
		Clock:              clock,
		Stats:              netstack.Metrics,
		HandleLocal:        true,
		RawFactory:         raw.EndpointFactory{},
	}), k.UniqueID())

	// Enable SACK.
	{
		opt := tcpip.TCPSACKEnabled(true)
		s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}
	// Set default TTL.
	{
		opt := tcpip.DefaultTTLOption(64)
		s.Stack.SetNetworkProtocolOption(ipv4.ProtocolNumber, &opt)
		s.Stack.SetNetworkProtocolOption(ipv6.ProtocolNumber, &opt)
	}
	// Enable moderate receive buffer.
	{
		opt := tcpip.TCPModerateReceiveBufferOption(true)
		s.Stack.SetTransportProtocolOption(tcp.ProtocolNumber, &opt)
	}

	// Create loopback NIC.
	nicID := tcpip.NICID(1)
	linkEP := ethernet.New(loopback.New())
	if err := s.Stack.CreateNICWithOptions(nicID, linkEP, stack.NICOptions{
		Name:               "lo",
		DeliverLinkPackets: true,
	}); err != nil {
		fatal("CreateNIC loopback: %v", err)
	}

	// Add loopback addresses.
	s.Stack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(net.IPv4(127, 0, 0, 1).To4()),
			PrefixLen: 8,
		},
	}, stack.AddressProperties{})

	s.Stack.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(net.IPv6loopback),
			PrefixLen: 128,
		},
	}, stack.AddressProperties{})

	// Set up route table. Start with loopback routes.
	lo4, _ := tcpip.NewSubnet(tcpip.AddrFromSlice([]byte{127, 0, 0, 0}), tcpip.MaskFromBytes([]byte{255, 0, 0, 0}))
	lo6, _ := tcpip.NewSubnet(tcpip.AddrFromSlice(net.IPv6loopback), tcpip.MaskFromBytes([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}))
	routes := []tcpip.Route{
		{Destination: lo4, NIC: nicID},
		{Destination: lo6, NIC: nicID},
	}

	// Add host networking NIC based on mode.
	switch netMode {
	case "utun":
		utunEP, err := utun.New()
		if err != nil {
			log.Warningf("utun: %v (continuing without host networking)", err)
			break
		}
		utunNIC := tcpip.NICID(2)
		if err := s.Stack.CreateNICWithOptions(utunNIC, utunEP, stack.NICOptions{
			Name: utunEP.Name(),
		}); err != nil {
			log.Warningf("CreateNIC utun: %v", err)
			break
		}
		unit := utunUnit(utunEP.Name())
		subnet3 := byte(100 + unit)
		hostIP := net.IPv4(192, 168, subnet3, 1).To4()
		guestIPv4 := net.IPv4(192, 168, subnet3, 2).To4()

		s.Stack.AddProtocolAddress(utunNIC, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(guestIPv4), PrefixLen: 30},
		}, stack.AddressProperties{})

		allIPv4, _ := tcpip.NewSubnet(tcpip.AddrFromSlice([]byte{0, 0, 0, 0}), tcpip.MaskFromBytes([]byte{0, 0, 0, 0}))
		routes = append(routes, tcpip.Route{
			Destination: allIPv4,
			Gateway:     tcpip.AddrFromSlice(hostIP),
			NIC:         utunNIC,
		})

		configureUtun(utunEP.Name(), hostIP, guestIPv4)
		utunEP.EnableProxy(guestIPv4, hostIP)

		if !*flagKeepRoot {
			if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
				var uid, gid int
				if _, err := fmt.Sscanf(sudoUID, "%d", &uid); err == nil && uid > 0 {
					if _, err := fmt.Sscanf(os.Getenv("SUDO_GID"), "%d", &gid); err == nil && gid > 0 {
						if err := unix.Setgid(gid); err != nil {
							fatal("setgid(%d): %v", gid, err)
						}
						if err := unix.Setgroups([]int{gid}); err != nil {
							log.Warningf("setgroups: %v", err)
						}
					}
					if err := unix.Setuid(uid); err != nil {
						fatal("setuid(%d): %v", uid, err)
					}
					log.Infof("Dropped privileges to uid=%d gid=%d", uid, gid)
				}
			}
		}
		log.Infof("Host networking enabled: %s (guest=%s, host=%s, proxy=on)", utunEP.Name(), guestIPv4, hostIP)

	case "vmnet":
		mac := vmnet.GenerateMAC(fmt.Sprintf("gvisor-%d", os.Getpid()))
		vmnetEP, err := vmnet.New(*flagVmnetSocket, mac)
		if err != nil {
			log.Warningf("vmnet: %v (continuing without host networking)", err)
			break
		}
		vmnetNIC := tcpip.NICID(2)
		if err := s.Stack.CreateNICWithOptions(vmnetNIC, vmnetEP, stack.NICOptions{
			Name: "vmnet0",
		}); err != nil {
			log.Warningf("CreateNIC vmnet: %v", err)
			break
		}

		guestIPv4 := net.ParseIP(*flagGuestIP).To4()
		if guestIPv4 == nil {
			fatal("invalid --guest-ip: %s", *flagGuestIP)
		}
		gatewayIP := net.IPv4(guestIPv4[0], guestIPv4[1], guestIPv4[2], 1).To4()

		s.Stack.AddProtocolAddress(vmnetNIC, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(guestIPv4), PrefixLen: 24},
		}, stack.AddressProperties{})

		allIPv4, _ := tcpip.NewSubnet(tcpip.AddrFromSlice([]byte{0, 0, 0, 0}), tcpip.MaskFromBytes([]byte{0, 0, 0, 0}))
		routes = append(routes, tcpip.Route{
			Destination: allIPv4,
			Gateway:     tcpip.AddrFromSlice(gatewayIP),
			NIC:         vmnetNIC,
		})

		// Write resolv.conf pointing to the vmnet gateway which runs a DNS forwarder.
		if *flagRootfs != "" {
			resolvPath := filepath.Join(*flagRootfs, "etc", "resolv.conf")
			os.WriteFile(resolvPath, []byte(fmt.Sprintf("nameserver %s\n", gatewayIP)), 0644)
		}

		log.Infof("Host networking enabled: vmnet (guest=%s, gateway=%s, mac=%s, no root required)", guestIPv4, gatewayIP, mac)

	case "proxy":
		guestIPv4 := net.IPv4(10, 0, 2, 15).To4()
		gatewayIP := net.IPv4(10, 0, 2, 2).To4()

		proxyEP := proxynet.New(guestIPv4, gatewayIP)
		proxyNIC := tcpip.NICID(2)
		if err := s.Stack.CreateNICWithOptions(proxyNIC, proxyEP, stack.NICOptions{
			Name: "proxy0",
		}); err != nil {
			log.Warningf("CreateNIC proxy: %v", err)
			break
		}

		s.Stack.AddProtocolAddress(proxyNIC, tcpip.ProtocolAddress{
			Protocol:          ipv4.ProtocolNumber,
			AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFromSlice(guestIPv4), PrefixLen: 24},
		}, stack.AddressProperties{})

		allIPv4, _ := tcpip.NewSubnet(tcpip.AddrFromSlice([]byte{0, 0, 0, 0}), tcpip.MaskFromBytes([]byte{0, 0, 0, 0}))
		routes = append(routes, tcpip.Route{
			Destination: allIPv4,
			Gateway:     tcpip.AddrFromSlice(gatewayIP),
			NIC:         proxyNIC,
		})

		if *flagRootfs != "" {
			resolvPath := filepath.Join(*flagRootfs, "etc", "resolv.conf")
			os.WriteFile(resolvPath, []byte("nameserver 8.8.8.8\n"), 0644)
		}

		log.Infof("Host networking enabled: proxy (guest=%s, no root, no daemon)", guestIPv4)
	}

	s.Stack.SetRouteTable(routes)
	return s
}

// configureUtun sets up the host side of the utun interface and NAT.
func configureUtun(ifname string, hostIP, guestIP net.IP) {
	// Assign IP address to the utun interface.
	out, err := exec.Command("ifconfig", ifname, "inet",
		fmt.Sprintf("%s/30", hostIP), guestIP.String()).CombinedOutput()
	if err != nil {
		log.Warningf("ifconfig %s: %v: %s", ifname, err, out)
		return
	}

	// Enable IP forwarding.
	out, err = exec.Command("sysctl", "-w", "net.inet.ip.forwarding=1").CombinedOutput()
	if err != nil {
		log.Warningf("sysctl ip.forwarding: %v: %s", err, out)
	}

	// Find the default outbound interface for NAT.
	outIface := "en0"
	if routeOut, err := exec.Command("route", "-n", "get", "default").Output(); err == nil {
		for _, line := range strings.Split(string(routeOut), "\n") {
			if strings.Contains(line, "interface:") {
				outIface = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
	}

	// Set up NAT with pfctl so the guest can reach the internet.
	subnet := fmt.Sprintf("%s/30", hostIP)
	pfRules := fmt.Sprintf("nat on %s from %s to any -> (%s)\npass all\n", outIface, subnet, outIface)
	pfCmd := exec.Command("pfctl", "-ef", "-")
	pfCmd.Stdin = strings.NewReader(pfRules)
	if out, err := pfCmd.CombinedOutput(); err != nil {
		log.Warningf("pfctl NAT: %v: %s", err, out)
	} else {
		log.Infof("NAT enabled: %s -> %s via %s", subnet, outIface, ifname)
	}
}

// utunUnit extracts the numeric unit from a utun interface name (e.g., "utun5" → 5).
func utunUnit(name string) int {
	n := 0
	for _, c := range name {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

// guestEnv returns environment variables for the guest process.
// It uses a standard Linux PATH instead of the host's macOS PATH.
func guestEnv() []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=xterm",
	}
	// Pass through select environment variables from host.
	for _, key := range []string{"LANG", "LC_ALL", "TZ"} {
		if val, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+val)
		}
	}
	return env
}

// setupGoferRoot creates an in-process gofer filesystem backed by the host
// directory and returns a mount namespace with it as the root.
func setupGoferRoot(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, hostDir string) *vfs.MountNamespace {
	// Register the gofer filesystem type.
	vfsObj.MustRegisterFilesystemType(goferfs.Name, &goferfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{
		AllowUserMount: true,
		AllowUserList:  true,
	})

	// Create a Unix socketpair for the lisafs connection.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		fatal("socketpair: %v", err)
	}
	// Increase socket buffer for heavy workloads (e.g., Python package install).
	unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	unix.SetsockoptInt(fds[0], unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)
	unix.SetsockoptInt(fds[1], unix.SOL_SOCKET, unix.SO_RCVBUF, 1<<20)
	unix.SetsockoptInt(fds[1], unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20)

	// Start the gofer server on one end.
	serverSock, err := unet.NewSocket(fds[0])
	if err != nil {
		fatal("unet.NewSocket: %v", err)
	}

	goferServer := fsgofer.NewLisafsServer(fsgofer.Config{
		DonateMountPointFD: *flagDirectfs,
	})
	conn, err := goferServer.CreateConnection(serverSock, hostDir, false /* readonly */)
	if err != nil {
		fatal("CreateConnection: %v", err)
	}
	goferServer.StartConnection(conn)

	// Create the mount namespace using the gofer filesystem with the
	// client socket FD.
	clientFD := fds[1]
	mountOpts := fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,cache=remote_revalidating", clientFD, clientFD)
	if *flagDirectfs {
		mountOpts += ",directfs"
	}
	mntns, err := vfsObj.NewMountNamespace(
		k.SupervisorContext(),
		creds,
		"none",
		goferfs.Name,
		&vfs.MountOptions{
			ReadOnly: false,
			GetFilesystemOptions: vfs.GetFilesystemOptions{
				Data: mountOpts,
				InternalData: goferfs.InternalFilesystemOptions{
					LeakConnection: true,
				},
			},
		},
		nil,
	)
	if err != nil {
		fatal("NewMountNamespace (gofer): %v", err)
	}
	return mntns
}

// openPTY allocates a PTY pair via devpts and returns the master and replica
// FileDescriptions plus the replica's kernel.TTY for job control.
func openPTY(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, root vfs.VirtualDentry, creds *auth.Credentials) (*vfs.FileDescription, *vfs.FileDescription, *kernel.TTY, error) {
	ctx := k.SupervisorContext()

	masterFD, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse("/dev/pts/ptmx"),
	}, &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NOCTTY})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open /dev/pts/ptmx: %w", err)
	}

	idx, replicaTTY, ok := devpts.PTYInfo(masterFD)
	if !ok {
		masterFD.DecRef(ctx)
		return nil, nil, nil, fmt.Errorf("not a PTY master")
	}

	slaveFD, err := vfsObj.OpenAt(ctx, creds, &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(fmt.Sprintf("/dev/pts/%d", idx)),
	}, &vfs.OpenOptions{Flags: linux.O_RDWR | linux.O_NOCTTY})
	if err != nil {
		masterFD.DecRef(ctx)
		return nil, nil, nil, fmt.Errorf("open /dev/pts/%d: %w", idx, err)
	}

	return masterFD, slaveFD, replicaTTY, nil
}

// pumpPTY copies data between host stdin/stdout and the PTY master.
// Runs until the master is closed (shell exits) or host stdin hits EOF.
func pumpPTY(k *kernel.Kernel, masterFD *vfs.FileDescription) {
	ctx := k.SupervisorContext()

	// Host stdin → PTY master (input to guest).
	// Uses a dup'd FD so we can close it to unblock the read goroutine
	// when the master side (output loop below) exits.
	stdinDup, _ := unix.Dup(0)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := unix.Read(stdinDup, buf)
			if n > 0 {
				data := buf[:n]
				for len(data) > 0 {
					src := usermem.BytesIOSequence(data)
					written, werr := masterFD.Write(ctx, src, vfs.WriteOptions{})
					if written > 0 {
						data = data[written:]
					}
					if werr != nil {
						break
					}
				}
			}
			if err != nil || n == 0 {
				break
			}
		}
	}()

	// When the output loop (below) exits, close the dup'd stdin FD
	// to unblock the input goroutine.
	defer unix.Close(stdinDup)

	// PTY master → host stdout (output from guest).
	e, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	masterFD.EventRegister(&e)
	defer masterFD.EventUnregister(&e)

	buf := make([]byte, 4096)
	for {
		dst := usermem.BytesIOSequence(buf)
		n, err := masterFD.Read(ctx, dst, vfs.ReadOptions{})
		if n > 0 {
			unix.Write(1, buf[:n])
		}
		if err != nil {
			if err == linuxerr.ErrWouldBlock {
				<-ch
				continue
			}
			break
		}
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sentry-darwin: "+format+"\n", args...)
	os.Exit(1)
}
