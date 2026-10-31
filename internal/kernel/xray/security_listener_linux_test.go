//go:build linux

package xray

import (
	"context"
	"net"
	"syscall"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
	"golang.org/x/sys/unix"
)

func TestUpstreamListenerDisablesMPTCPByDefault(t *testing.T) {
	t.Setenv("GODEBUG", "multipathtcp=2")
	for _, tc := range []struct {
		name    string
		options *internet.SocketConfig
	}{
		{"no_socket_options", nil},
		{"empty_socket_options", &internet.SocketConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dl internet.DefaultListener
			ln, err := dl.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, tc.options)
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
				t.Fatalf("Xray listener socket protocol = %d, want ordinary TCP (%d)", protocol, unix.IPPROTO_TCP)
			}
		})
	}
}
