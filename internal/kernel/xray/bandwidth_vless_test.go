package xray

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/limiter"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestXrayVLESSTCPRateLimitsEachDirection(t *testing.T) {
	target, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	targetPort := target.Addr().(*net.TCPAddr).Port
	inboundPort := reserveFreeTCPPort(t)
	node := &model.NodeSpec{
		Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: inboundPort, Network: "tcp",
		CustomRoutes: []M{{"type": "field", "ip": []string{"127.0.0.1/32"}, "port": fmt.Sprint(targetPort), "outboundTag": "direct"}},
	}
	x := New(config.KernelConfig{Type: "xray", LogLevel: "error", CustomOutbound: []M{{
		"tag": "direct", "protocol": "freedom",
		"settings": M{"finalRules": []M{{"action": "allow", "ip": []string{"127.0.0.1/32"}}}},
	}}})
	// Each direction gets a fresh user's full bucket, wired as in the node service.
	users := []model.UserSpec{testUsers[0], testUsers[1]}
	for i := range users {
		users[i].SpeedLimit = 1
	}
	limits := limiter.New()
	limits.UpdateUsers(users)
	tracker := limiter.NewSpeedTracker(limits)
	tracker.UpdateBuckets()
	x.SetSpeedLimitFunc(tracker.GetLimiter)
	if err := x.Start(node, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer x.Stop()

	const bytesPerSecond = 1_000_000 / 8
	payload := make([]byte, 2*bytesPerSecond)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for i, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			client, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", inboundPort), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			deadline := time.Now().Add(10 * time.Second)
			client.SetDeadline(deadline)
			target.SetDeadline(deadline)
			// Complete both VLESS headers before starting the transfer clock.
			if _, err := client.Write(buildVLESSv0Handshake(t, users[i].UUID, targetPort, []byte{'?'})); err != nil {
				t.Fatal(err)
			}
			peer, err := target.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			peer.SetDeadline(deadline)
			var marker [1]byte
			if _, err := io.ReadFull(peer, marker[:]); err != nil || marker[0] != '?' {
				t.Fatalf("VLESS handshake payload = %q, err = %v", marker, err)
			}
			if _, err := peer.Write([]byte{'!'}); err != nil {
				t.Fatal(err)
			}
			if err := readVLESSResponseHeader(t, client); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(client, marker[:]); err != nil || marker[0] != '!' {
				t.Fatalf("VLESS handshake reply = %q, err = %v", marker, err)
			}

			sender, receiver := net.Conn(client), net.Conn(peer)
			if direction == "download" {
				sender, receiver = receiver, sender
			}
			got := make([]byte, len(payload))
			written := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := bytes.NewReader(payload).WriteTo(sender)
				written <- err
			}()
			n, err := io.ReadFull(receiver, got)
			elapsed := time.Since(start)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("received %d/%d bytes, err = %v, matches = %v", n, len(payload), err, bytes.Equal(got, payload))
			}
			if err := <-written; err != nil {
				t.Fatal(err)
			}
			// At 1 Mbps the first 125 kB fit in the burst; the second need one second.
			const minElapsed = 850 * time.Millisecond
			if elapsed < minElapsed {
				t.Fatalf("received %d bytes in %v; expected at least %v", n, elapsed, minElapsed)
			}
			t.Logf("received %d bytes in %v", n, elapsed)
		})
	}
}
