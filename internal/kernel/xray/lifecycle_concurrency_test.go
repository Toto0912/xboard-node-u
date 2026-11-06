package xray

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestConcurrentAdmissionAndStatistics(t *testing.T) {
	d := newTestDispatcher()
	d.UpdateLimits(map[string]int{"user": 1}, map[string]int{"user": 4}, nil)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			ip := fmt.Sprintf("192.0.2.%d", worker+1)
			for i := 0; i < 3000; i++ {
				if !d.checkDeviceLimit("user", ip, true) {
					d.delConn("user", ip)
				}
			}
		}(worker)
	}
	for i := 0; i < 3000; i++ {
		d.GetConnectionState()
	}
	wg.Wait()
	alive, _ := d.GetConnectionState()
	if len(alive) != 0 {
		t.Fatalf("connections did not drain: %v", alive)
	}
}

func TestStopReleasesListenerBeforeReturning(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray", LogLevel: "error"})
	node := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), Network: "tcp"}
	if err := x.Start(node, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer x.Stop()
	// Simulate an established stream that has not completed when Stop begins.
	x.limitDispatcher.connCount.Store(1)
	started := time.Now()
	x.Stop()
	if time.Since(started) > 3*time.Second {
		t.Fatal("Stop waited for stream drain before closing listeners")
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", node.ServerPort))
	if err != nil {
		t.Fatalf("Stop left listener active: %v", err)
	}
	listener.Close()
	if err := x.Start(node, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatalf("restart after Stop: %v", err)
	}
}
