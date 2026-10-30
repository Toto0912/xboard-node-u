package singbox

import (
	"fmt"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"io"
	"net"
	"testing"
	"time"
)

func TestUpgradeEmptySOCKSUsersRejectAnonymous(t *testing.T) {
	s := New(config.KernelConfig{LogLevel: "error"})
	defer s.Stop()
	n := &model.NodeSpec{Protocol: "socks", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t)}
	if err := s.Start(n, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveUsers(testUsers[:1]); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", n.ServerPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 255 {
		t.Fatalf("empty users enabled anonymous proxy: %v", reply)
	}
	if _, err := s.AddUsers(testUsers[:1]); err != nil {
		t.Fatal(err)
	}
}
