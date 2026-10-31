//go:build linux

package main

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHealthListenerDisablesMPTCP(t *testing.T) {
	// Force Go's implicit listener default on; our explicit override must win.
	t.Setenv("GODEBUG", "multipathtcp=2")
	ln, err := listenHealth("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	raw, err := ln.(syscall.Conn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var protocol int
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		protocol, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
	}); err != nil {
		t.Fatal(err)
	}
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	if protocol != unix.IPPROTO_TCP {
		t.Fatalf("health listener socket protocol = %d, want ordinary TCP (%d)", protocol, unix.IPPROTO_TCP)
	}
}
