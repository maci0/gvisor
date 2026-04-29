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
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"runtime"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
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
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/fsgofer"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/loader"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf"
	_ "gvisor.dev/gvisor/pkg/sentry/platform/platforms"
	"gvisor.dev/gvisor/pkg/sentry/socket/netstack"
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netstack"        // AF_INET provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink"         // AF_NETLINK provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/route"   // NETLINK_ROUTE provider
	_ "gvisor.dev/gvisor/pkg/sentry/socket/netlink/uevent"  // NETLINK_KOBJECT_UEVENT
	_ "gvisor.dev/gvisor/pkg/sentry/socket/unix"            // AF_UNIX provider
	"gvisor.dev/gvisor/pkg/sentry/strace"
	_ "gvisor.dev/gvisor/pkg/sentry/syscalls/linux" // register syscall table
	"gvisor.dev/gvisor/pkg/sentry/time"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/link/loopback"
	"gvisor.dev/gvisor/pkg/tcpip/link/utun"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	flagStrace   = flag.Bool("strace", false, "enable system call tracing")
	flagRootfs   = flag.String("rootfs", "", "host directory to copy into guest root filesystem")
	flagNet      = flag.Bool("net", false, "enable host networking via utun (requires root)")
	flagCPUs     = flag.Int("cpus", 0, "number of vCPUs (0 = auto-detect)")
	flagKeepRoot = flag.Bool("keep-root", false, "don't drop root privileges after network setup")
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [-strace] [-rootfs dir] <static-linux-elf> [args...]\n", os.Args[0])
		os.Exit(1)
	}

	log.SetLevel(log.Warning)

	// Configure ARM64 address space for VA36 (64GB) to fit within
	// HVF's IPA range without guest MMU page tables.
	arch.ConfigureAddressSpace(1 << 36)

	elfPath := flag.Arg(0)
	guestArgs := flag.Args()

	// Initialize memory usage tracking.
	if err := usage.Init(); err != nil {
		fatal("usage.Init: %v", err)
	}

	// Create the HVF platform.
	constructor, err := platform.Lookup("hvf")
	if err != nil {
		fatal("platform lookup: %v", err)
	}
	plat, err := constructor.New(platform.Options{})
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

	// Prepare VDSO (empty on macOS - no VDSO binary available).
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

	// Create FD table and import stdin/stdout/stderr from host.
	ctx := k.SupervisorContext()
	fdTable := k.NewFDTable()

	for hostFD := 0; hostFD < 3; hostFD++ {
		newFD, err := unix.Dup(hostFD)
		if err != nil {
			fatal("dup fd %d: %v", hostFD, err)
		}
		f, err := host.NewFD(ctx, k.HostMount(), newFD, &host.NewFDOptions{})
		if err != nil {
			unix.Close(newFD)
			fatal("host.NewFD for stdio %d: %v", hostFD, err)
		}
		if _, err := fdTable.NewFDAt(ctx, int32(hostFD), f, kernel.FDFlags{}); err != nil {
			f.DecRef(ctx)
			fatal("NewFDAt %d: %v", hostFD, err)
		}
		f.DecRef(ctx)
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
	}

	// If stdin is a terminal and no "-c" flag is passed (i.e., the user
	// is launching an interactive shell), set raw mode for proper terminal
	// pass-through including line editing and signal characters.
	hasCmd := false
	for _, a := range guestArgs {
		if a == "-c" {
			hasCmd = true
			break
		}
	}
	var termState *unix.Termios
	_, isattyErr := unix.IoctlGetTermios(0, unix.TIOCGETA)
	if isattyErr == nil && !hasCmd {
		var err error
		termState, err = unix.IoctlGetTermios(0, unix.TIOCGETA)
		if err == nil {
			raw := *termState
			raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
			raw.Oflag &^= unix.OPOST
			raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
			raw.Cflag &^= unix.CSIZE | unix.PARENB
			raw.Cflag |= unix.CS8
			raw.Cc[unix.VMIN] = 1
			raw.Cc[unix.VTIME] = 0
			unix.IoctlSetTermios(0, unix.TIOCSETA, &raw)
		}
	}

	log.Infof("Starting gVisor sentry kernel on macOS (HVF platform, %d CPUs)", numCPU)

	// Start the kernel.
	if err := k.Start(); err != nil {
		fatal("kernel.Start: %v", err)
	}

	// Forward host signals to the guest init process.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM, unix.SIGHUP)
	go func() {
		for sig := range sigCh {
			s := sig.(unix.Signal)
			log.Infof("Received signal %d, terminating guest", s)
			k.SendExternalSignal(&linux.SignalInfo{Signo: int32(s)}, "host")
			// If the init process has the signal masked (e.g., shell
			// waiting for a child), also kill all tasks directly.
			k.TaskSet().Kill(linux.WaitStatusTerminationSignal(linux.Signal(s)))
		}
	}()

	tg.WaitExited()

	// Restore terminal if we changed it.
	if termState != nil {
		unix.IoctlSetTermios(0, unix.TIOCSETA, termState)
	}

	exitStatus := tg.ExitStatus()
	log.Infof("Guest exited with status %d", exitStatus.ExitStatus())
	// Sync stdout/stderr to flush any pending guest output through host
	// pipes before exit. Without this, os.Exit can race with in-flight
	// writes on other goroutines.
	unix.Fsync(1)
	unix.Fsync(2)
	os.Exit(int(exitStatus.ExitStatus()))
}

// createNetworkStack creates a gVisor netstack with a loopback interface
// and optionally a utun interface for host networking.
func createNetworkStack(clock tcpip.Clock, k *kernel.Kernel, enableUtun bool) *netstack.Stack {
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

	// Add utun NIC if host networking is enabled.
	if enableUtun {
		utunEP, err := utun.New()
		if err != nil {
			log.Warningf("utun: %v (continuing without host networking)", err)
		} else {
			utunNIC := tcpip.NICID(2)
			if err := s.Stack.CreateNICWithOptions(utunNIC, utunEP, stack.NICOptions{
				Name: utunEP.Name(),
			}); err != nil {
				log.Warningf("CreateNIC utun: %v", err)
			} else {
				// Derive a unique /30 subnet from the utun unit number
				// to avoid conflicts between multiple gVisor instances.
				// utunN → 192.168.(100+N).1/30, guest .2
				unit := utunUnit(utunEP.Name())
				subnet3 := byte(100 + unit)
				hostIP := net.IPv4(192, 168, subnet3, 1).To4()
				guestIPv4 := net.IPv4(192, 168, subnet3, 2).To4()

				guestIP := tcpip.AddrFromSlice(guestIPv4)
				s.Stack.AddProtocolAddress(utunNIC, tcpip.ProtocolAddress{
					Protocol:          ipv4.ProtocolNumber,
					AddressWithPrefix: tcpip.AddressWithPrefix{Address: guestIP, PrefixLen: 30},
				}, stack.AddressProperties{})

				// Default route via utun for non-loopback traffic.
				allIPv4, _ := tcpip.NewSubnet(tcpip.AddrFromSlice([]byte{0, 0, 0, 0}), tcpip.MaskFromBytes([]byte{0, 0, 0, 0}))
				routes = append(routes, tcpip.Route{
					Destination: allIPv4,
					Gateway:     tcpip.AddrFromSlice(hostIP),
					NIC:         utunNIC,
				})

				// Configure host side of the utun interface.
				configureUtun(utunEP.Name(), hostIP, guestIPv4)

				// Enable userspace NAT proxy for internet access.
				// pfctl NAT doesn't route de-NAT'd replies on macOS.
				utunEP.EnableProxy(guestIPv4, hostIP)

				// Drop root privileges unless --keep-root is set.
				if !*flagKeepRoot {
				if sudoUID := os.Getenv("SUDO_UID"); sudoUID != "" {
					var uid, gid int
					fmt.Sscanf(sudoUID, "%d", &uid)
					fmt.Sscanf(os.Getenv("SUDO_GID"), "%d", &gid)
					if uid > 0 {
						if gid > 0 {
							unix.Setgid(gid)
							unix.Setgroups([]int{gid})
						}
						unix.Setuid(uid)
						log.Infof("Dropped root privileges to uid=%d gid=%d", uid, gid)
					}
				}
				}

				log.Infof("Host networking enabled: %s (guest=%s, host=%s, proxy=on)", utunEP.Name(), guestIPv4, hostIP)
			}
		}
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

// copyHostDirToGuest recursively copies a host directory into the guest's
// tmpfs root filesystem. This provides a simple host filesystem passthrough
// without requiring a gofer process.
func copyHostDirToGuest(k *kernel.Kernel, vfsObj *vfs.VirtualFilesystem, creds *auth.Credentials, root vfs.VirtualDentry, hostDir string) {
	ctx := k.SupervisorContext()
	log.Infof("Copying host directory %s into guest root", hostDir)

	filepath.WalkDir(hostDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip errors
		}

		// Compute guest path relative to the host directory.
		rel, _ := filepath.Rel(hostDir, path)
		if rel == "." {
			return nil
		}
		guestPath := "/" + rel

		pop := vfs.PathOperation{
			Root:  root,
			Start: root,
			Path:  fspath.Parse(guestPath),
		}

		if d.IsDir() {
			vfsObj.MkdirAt(ctx, creds, &pop, &vfs.MkdirOptions{Mode: 0755})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		// Handle symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return nil
			}
			vfsObj.SymlinkAt(ctx, creds, &pop, target)
			return nil
		}

		// Regular files only.
		if !d.Type().IsRegular() {
			return nil
		}

		// Read file from host.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Determine permissions.
		mode := uint16(info.Mode().Perm())

		// Create file in guest.
		fd, err := vfsObj.OpenAt(ctx, creds, &pop, &vfs.OpenOptions{
			Flags: linux.O_WRONLY | linux.O_CREAT | linux.O_TRUNC,
			Mode:  linux.FileMode(mode),
		})
		if err != nil {
			log.Debugf("create %s: %v", guestPath, err)
			return nil
		}
		defer fd.DecRef(ctx)

		// Write data in chunks.
		for written := 0; written < len(data); {
			chunk := data[written:]
			if len(chunk) > 65536 {
				chunk = chunk[:65536]
			}
			dst := usermem.BytesIOSequence(chunk)
			n, err := fd.Write(ctx, dst, vfs.WriteOptions{})
			if n == 0 || err != nil {
				break
			}
			written += int(n)
		}

		return nil
	})
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

	goferServer := fsgofer.NewLisafsServer(fsgofer.Config{})
	conn, err := goferServer.CreateConnection(serverSock, hostDir, false /* readonly */)
	if err != nil {
		fatal("CreateConnection: %v", err)
	}
	goferServer.StartConnection(conn)

	// Create the mount namespace using the gofer filesystem with the
	// client socket FD.
	clientFD := fds[1]
	mountOpts := fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,cache=remote_revalidating", clientFD, clientFD)
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sentry-darwin: "+format+"\n", args...)
	os.Exit(1)
}
