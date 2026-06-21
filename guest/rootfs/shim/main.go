// SPDX-License-Identifier: Apache-2.0
//
// guest-proxy-shim — the guest-side egress shim baked into the read-only rootfs base
// (ADR 010 Q4). It listens on /proxy.sock inside the guest and pumps every byte, in both
// directions, over AF_VSOCK to the host bridge (CID 2 / a fixed port), which terminates at the
// live host-side EgressProxy. It is a DUMB byte pump by construction: it imports no HTTP package,
// holds no host allowlist or method map, and stores no secret — so it makes no egress decision and
// cannot leak a credential it never holds. The egress decision (which hosts, which verbs, which
// secret to inject) is made host-side by the EgressProxy after the vsock hop; the credential is
// injected there and never crosses into the guest.
//
// This is the booted-guest realization of vsockshim.go's startGuestShim/relay: the same
// transparent bidirectional pump, with the dialVsock seam concretized as an AF_VSOCK dial. It is a
// SEPARATE main package (built CGO_ENABLED=0 static, vendored into base.ext4) so it never links
// into the host exec-sandbox binary and carries no host-side surface.
package main

import (
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

// The vsock rendezvous the host bridge fronts: the guest dials the host (CID 2) on this port;
// firecracker forwards that connection to the host-side unix socket <uds_path>_<port>, where the
// vsockBridge listens and relays to the EgressProxy. Must match hostVsockPort in the backend.
const (
	hostCID   = 2 // VMADDR_CID_HOST — the host the guest reaches over vsock
	proxyPort = 1024
)

func main() {
	sockPath := "/proxy.sock"
	if len(os.Args) > 1 && os.Args[1] != "" {
		sockPath = os.Args[1]
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		log.Fatalf("guest-proxy-shim: listen %s: %v", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o666)
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(client)
	}
}

func handle(client net.Conn) {
	defer client.Close()
	up, err := dialVsock(hostCID, proxyPort)
	if err != nil {
		return // host bridge unreachable; drop (fail closed — no egress without the bridge)
	}
	defer up.Close()
	relay(client, up)
}

// relay copies bytes between the two ends in both directions until either closes — a transparent,
// byte-exact pump (no buffering-and-parsing, no rewriting, no dropping), mirroring vsockshim.go.
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// --- AF_VSOCK dial (stdlib syscall only; no third-party vsock package) ---

// sockaddrVM mirrors struct sockaddr_vm (linux/vm_sockets.h).
type sockaddrVM struct {
	family   uint16
	reserved uint16
	port     uint32
	cid      uint32
	zero     [4]byte
}

const afVSOCK = 40 // AF_VSOCK

// dialVsock opens an AF_VSOCK stream to (cid, port) and wraps the fd as a net.Conn. It uses only
// the stdlib syscall package — no cgo, no third-party vsock library — so the shim stays static and
// dependency-free. The returned conn supports CloseWrite (half-close) via os.File→net semantics.
func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	sa := sockaddrVM{family: afVSOCK, port: port, cid: cid}
	_, _, errno := syscall.Syscall(
		syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
	if errno != 0 {
		syscall.Close(fd)
		return nil, errno
	}
	f := os.NewFile(uintptr(fd), "vsock:"+strconv.Itoa(int(cid))+":"+strconv.Itoa(int(port)))
	c, err := net.FileConn(f)
	f.Close() // FileConn dups the fd; close ours
	return c, err
}
