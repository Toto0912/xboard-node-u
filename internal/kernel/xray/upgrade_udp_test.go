package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	"net"
	"testing"
	"time"
)

func TestUpgradeXUDP(t *testing.T) {
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, a, err := echo.ReadFrom(buffer)
			if err != nil {
				return
			}
			echo.WriteTo(buffer[:n], a)
		}
	}()
	n := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), CustomRoutes: []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}}
	s := New(config.KernelConfig{Type: "xray", LogLevel: "error", CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}})
	defer s.Stop()
	if err := s.Start(n, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"log": map[string]any{"loglevel": "error"}, "outbounds": []any{map[string]any{"protocol": "vless", "settings": map[string]any{"packetEncoding": "xudp", "vnext": []any{map[string]any{"address": "127.0.0.1", "port": n.ServerPort, "users": []any{map[string]any{"id": testUsers[0].UUID, "encryption": "none"}}}}}, "mux": map[string]any{"enabled": true, "concurrency": 4, "xudpConcurrency": 2}}}})
	pb, err := serial.LoadJSONConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	client, err := xcore.New(pb)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	conn, err := xcore.DialUDP(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 3; i++ {
		payload := []byte("xudp-upgrade-test")
		if _, err := conn.WriteTo(payload, echo.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 100)
		n, _, err := conn.ReadFrom(buffer)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(payload, buffer[:n]) {
			t.Fatal("UDP payload mismatch")
		}
	}
	traffic, _, _, _ := s.GetUserTraffic(context.Background())
	if traffic[testUsers[0].ID][0] == 0 || traffic[testUsers[0].ID][1] == 0 {
		t.Fatalf("missing UDP traffic: %v", traffic)
	}
}
