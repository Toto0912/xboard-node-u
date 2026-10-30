package xray

import (
	"fmt"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"net"
	"testing"
	"time"
)

func TestReloadSamePortAndRollback(t *testing.T) {
	s := New(config.KernelConfig{Type: "xray", LogLevel: "error"})
	defer s.Stop()
	n := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), Network: "xhttp", NetworkSettings: map[string]any{"path": "/before"}}
	if err := s.Start(n, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	changed := *n
	changed.NetworkSettings = map[string]any{"path": "/after"}
	if err := s.Reload(&changed, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal("same-port reload", err)
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	bad := changed
	bad.ServerPort = busy.Addr().(*net.TCPAddr).Port
	if err := s.Reload(&bad, testUsers[:1], kernel.TLSCert{}); err == nil {
		t.Fatal("expected port conflict")
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", n.ServerPort), time.Second)
	if err != nil {
		t.Fatal("old port was not restored", err)
	}
	c.Close()
}
