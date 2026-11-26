package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	J "github.com/sagernet/sing/common/json"
	singM "github.com/sagernet/sing/common/metadata"
)

func TestMieruBindsSelectedAddressAndRestarts(t *testing.T) {
	for _, transport := range []string{"TCP", "UDP"} {
		t.Run(transport, func(t *testing.T) {
			open := func(host string, port int) (io.Closer, int, error) {
				addr := net.JoinHostPort(host, strconv.Itoa(port))
				if transport == "TCP" {
					ln, err := net.Listen("tcp4", addr)
					if err != nil {
						return nil, 0, err
					}
					return ln, ln.Addr().(*net.TCPAddr).Port, nil
				}
				ln, err := net.ListenPacket("udp4", addr)
				if err != nil {
					return nil, 0, err
				}
				return ln, ln.LocalAddr().(*net.UDPAddr).Port, nil
			}
			blocker, port, err := open("127.0.0.1", 0)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Close()
			other, _, err := open("127.0.0.2", port)
			if err != nil {
				t.Skipf("second loopback address unavailable: %v", err)
			}
			defer other.Close()
			user := testUsers[0]
			spec := &model.NodeSpec{Protocol: "mieru", Transport: transport, ServerPort: port, ListenIP: "127.0.0.1",
				CustomRoutes: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}}
			server := New(config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir(), LogLevel: "error"})
			defer server.Stop()
			start := func() error { return server.Start(spec, []model.UserSpec{user}, kernel.TLSCert{}) }
			if start() == nil {
				t.Fatal("occupied Mieru listener reported successful startup")
			}
			if err := blocker.Close(); err != nil {
				t.Fatal(err)
			}
			if err := start(); err != nil {
				t.Fatal(err)
			}

			ctx := include.Context(context.Background())
			raw, err := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "outbounds": []any{
				map[string]any{"type": "mieru", "tag": "proxy", "server": "127.0.0.1", "server_port": port, "transport": transport,
					"username": user.UUID, "password": user.UUID}}})
			if err != nil {
				t.Fatal(err)
			}
			opts, err := J.UnmarshalExtendedContext[option.Options](ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			client, err := box.New(box.Options{Context: ctx, Options: opts})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			out, ok := client.Outbound().Outbound("proxy")
			if !ok {
				t.Fatal("missing client outbound")
			}
			udp := udpLoopback(t)
			dest := singM.SocksaddrFromNet(udp.LocalAddr())
			if _, err := out.ListenPacket(ctx, dest); err == nil {
				t.Fatal("client dialed before lifecycle startup")
			}
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 1024)
			go func() {
				for {
					n, a, err := udp.ReadFromUDP(buf)
					if err != nil {
						return
					}
					if _, err = udp.WriteToUDP(buf[:n], a); err != nil {
						return
					}
				}
			}()
			pc, err := out.ListenPacket(ctx, dest)
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			var total int64
			reply := make([]byte, 1024)
			for i := 1; i <= 32; i++ {
				want := bytes.Repeat([]byte{byte(i)}, i+11)
				pc.SetDeadline(time.Now().Add(8 * time.Second))
				if _, err = pc.WriteTo(want, udp.LocalAddr()); err != nil {
					t.Fatal(err)
				}
				n, from, err := pc.ReadFrom(reply)
				if err != nil || !bytes.Equal(want, reply[:n]) || from.String() != udp.LocalAddr().String() {
					t.Fatalf("UDP reply %d from %v, %v", n, from, err)
				}
				total += int64(n)
			}
			traffic, _, _, err := server.GetUserTraffic(ctx)
			if err != nil || traffic[user.ID] != [2]int64{total, total} {
				t.Fatalf("traffic %v, expected %d each way, err=%v", traffic, total, err)
			}
			pc.Close()
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			if p, err := out.ListenPacket(ctx, dest); err == nil {
				p.Close()
				t.Fatal("Mieru outbound remains active after close")
			}
			server.Stop()
			rebound, _, err := open("127.0.0.1", port)
			if err != nil {
				t.Fatal(fmt.Errorf("port still in use after close: %w", err))
			}
			rebound.Close()
		})
	}
}
