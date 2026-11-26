package kernel_test

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/kernel/singbox"
	"github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/model"
)

func localListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// Check SOCKS password authentication on the live listener, without dialing
// an outbound target. Both embedded kernels use the user's UUID as credentials.
func acceptsPassword(t *testing.T, port int, password string) bool {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var reply [2]byte
	if _, err := conn.Write([]byte{5, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, reply[:]); err != nil || reply != [2]byte{5, 2} {
		t.Fatalf("password authentication not negotiated: %v, %v", reply, err)
	}
	credentials := append([]byte{1, byte(len(password))}, password...)
	credentials = append(credentials, byte(len(password)))
	credentials = append(credentials, password...)
	if _, err := conn.Write(credentials); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadFull(conn, reply[:])
	return err == nil && reply == [2]byte{1, 0}
}

func TestKernelRollbackOnlyAcceptsRetainedCredentials(t *testing.T) {
	kernels := map[string]func() kernel.Kernel{
		"xray":    func() kernel.Kernel { return xray.New(config.KernelConfig{LogLevel: "error"}) },
		"singbox": func() kernel.Kernel { return singbox.New(config.KernelConfig{LogLevel: "error"}) },
	}
	for name, create := range kernels {
		t.Run(name, func(t *testing.T) {
			k := create()
			t.Cleanup(k.Stop)
			blocked := localListener(t)
			defer blocked.Close()
			reservation := localListener(t)
			node := model.NodeSpec{Protocol: "socks", ListenIP: "127.0.0.1", Network: "tcp", ServerPort: reservation.Addr().(*net.TCPAddr).Port}
			reservation.Close()
			a := model.UserSpec{ID: 1, UUID: "retired"}
			b := model.UserSpec{ID: 2, UUID: "retained"}
			replacement := model.UserSpec{ID: 1, UUID: "replacement"}
			if err := k.Start(&node, []model.UserSpec{a, b}, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			if !acceptsPassword(t, node.ServerPort, a.UUID) || !acceptsPassword(t, node.ServerPort, b.UUID) {
				t.Fatal("initial credentials were not active")
			}
			candidate := node
			candidate.ServerPort = blocked.Addr().(*net.TCPAddr).Port
			if err := k.Reload(&candidate, []model.UserSpec{replacement, b}, kernel.TLSCert{}); err == nil {
				t.Fatal("reloading on an occupied port succeeded")
			}
			if !acceptsPassword(t, node.ServerPort, b.UUID) || acceptsPassword(t, node.ServerPort, a.UUID) || acceptsPassword(t, node.ServerPort, replacement.UUID) {
				t.Fatal("rollback must serve only the credential present in both lists")
			}
			if err := k.Reload(&candidate, []model.UserSpec{replacement}, kernel.TLSCert{}); err == nil || k.IsRunning() {
				t.Fatalf("an empty rollback intersection should leave the listener stopped: %v", err)
			}
		})
	}
}
