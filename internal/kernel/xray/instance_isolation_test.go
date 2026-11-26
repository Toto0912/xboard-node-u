package xray

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/proxy/socks"
	"golang.org/x/net/proxy"
)

// Both backends answer on the same port. The marker identifies which backend
// actually received the connection, even when using the wrong peer still works.
func instanceTestBackend(t *testing.T, address string, marker byte) int {
	t.Helper()
	l, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req [1]byte
				for {
					if _, err := io.ReadFull(c, req[:]); err != nil {
						return
					}
					if _, err := c.Write([]byte{marker}); err != nil {
						return
					}
				}
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func instanceTestMarker(c net.Conn, want byte) error {
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte{'?'}); err != nil {
		return err
	}
	var got [1]byte
	if _, err := io.ReadFull(c, got[:]); err != nil {
		return err
	}
	if got[0] != want {
		return fmt.Errorf("flow reached backend %c, want %c", got[0], want)
	}
	return nil
}

func instanceTestUDPBackend(t *testing.T, ip string, port int, marker byte) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		var req [1]byte
		for {
			_, from, err := c.ReadFromUDP(req[:])
			if err != nil {
				return
			}
			c.WriteToUDP([]byte{marker}, from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func instanceTestUDPFlow(t *testing.T, inbound, backend int, domain string, want byte) {
	t.Helper()
	ctl, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(inbound)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ctl.SetDeadline(time.Now().Add(3 * time.Second))
	assoc, err := socks.ClientHandshake(&protocol.RequestHeader{Version: 5, Command: protocol.RequestCommandUDP,
		User: &protocol.MemoryUser{Account: &socks.Account{Username: testUsers[0].UUID, Password: testUsers[0].UUID}},
	}, ctl, ctl)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: assoc.Address.IP(), Port: int(assoc.Port)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Establish the association with an IP first. The subsequent SOCKS datagram
	// carries a domain, requiring Freedom's UDP writer to resolve its packet destination.
	for _, p := range []struct {
		dest   string
		marker byte
	}{{"127.0.0.1", 'A'}, {domain, want}} {
		req := &protocol.RequestHeader{Address: xnet.ParseAddress(p.dest), Port: xnet.Port(backend)}
		msg, err := socks.EncodeUDPPacket(req, []byte{'?'})
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		_, err = c.Write(msg.Bytes())
		msg.Release()
		if err != nil {
			t.Fatal(err)
		}
		var data [128]byte
		n, err := c.Read(data[:])
		if err != nil {
			t.Fatalf("UDP %s: %v", p.dest, err)
		}
		response := buf.FromBytes(data[:n])
		_, err = socks.DecodeUDPPacket(response)
		got := string(response.Bytes())
		response.Release()
		if err != nil || got != string(p.marker) {
			t.Errorf("UDP %s reached %q, want %c: %v", p.dest, got, p.marker, err)
		}
	}
}

func TestXrayInstanceDialerIsolation(t *testing.T) {
	port := instanceTestBackend(t, "127.0.0.1:0", 'A')
	instanceTestBackend(t, net.JoinHostPort("127.0.0.2", strconv.Itoa(port)), 'B')
	for _, mode := range []string{"socket-DNS", "freedom-DNS", "dialerProxy", "freedom-UDP-DNS"} {
		t.Run(mode, func(t *testing.T) {
			const domain = "xray-sibling.invalid"
			target := net.JoinHostPort(domain, strconv.Itoa(port))
			if mode == "dialerProxy" {
				target = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			}
			start := func(ip string) (*Xray, proxy.ContextDialer) {
				t.Helper()
				k := config.KernelConfig{Type: "xray", LogLevel: "error", ConfigDir: t.TempDir()}
				var outbounds []model.OutboundConfig
				allow := func(tag string) model.OutboundConfig {
					return model.OutboundConfig{Tag: tag, Protocol: "freedom", Settings: map[string]any{
						"finalRules": []map[string]any{{"action": "allow", "ip": []string{"127.0.0.0/8"}}},
					}}
				}
				direct := allow("direct")
				if mode == "dialerProxy" {
					// Both instances have the same outbound name but forward to different listeners.
					direct.Extra = map[string]any{"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "egress"}}}
					egress := allow("egress")
					egress.Settings["redirect"] = net.JoinHostPort(ip, strconv.Itoa(port))
					outbounds = []model.OutboundConfig{direct, egress}
				} else {
					k.CustomConfig = filepath.Join(k.ConfigDir, "dns.json")
					raw := fmt.Sprintf(`{"dns":{"hosts":{%q:%q},"servers":["localhost"]}}`, domain, ip)
					if err := os.WriteFile(k.CustomConfig, []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					if mode == "socket-DNS" {
						direct.Extra = map[string]any{"streamSettings": map[string]any{"sockopt": map[string]any{"domainStrategy": "ForceIPv4"}}}
					} else {
						direct.Settings["domainStrategy"] = "ForceIPv4"
					}
					outbounds = []model.OutboundConfig{direct}
				}
				spec := &model.NodeSpec{
					Protocol: "socks", ListenIP: "127.0.0.1", ServerPort: freeTestPort(t),
					CustomOutbounds: outbounds,
					CustomRoutes: []map[string]any{
						{"type": "field", "ip": []string{"127.0.0.0/8"}, "outboundTag": "direct"},
						{"type": "field", "domain": []string{"full:" + domain}, "outboundTag": "direct"},
					},
				}
				s := New(k)
				if err := s.Start(spec, testUsers[:1], kernel.TLSCert{}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(s.Stop)
				auth := &proxy.Auth{User: testUsers[0].UUID, Password: testUsers[0].UUID}
				d, err := proxy.SOCKS5("tcp", net.JoinHostPort(spec.ListenIP, strconv.Itoa(spec.ServerPort)), auth, nil)
				if err != nil {
					t.Fatal(err)
				}
				return s, d.(proxy.ContextDialer)
			}
			connect := func(d proxy.ContextDialer) (net.Conn, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				return d.DialContext(ctx, "tcp", target)
			}
			fresh := func(d proxy.ContextDialer, want byte) error {
				c, err := connect(d)
				if err != nil {
					return err
				}
				defer c.Close()
				return instanceTestMarker(c, want)
			}
			aKernel, a := start("127.0.0.1")
			if mode == "freedom-UDP-DNS" {
				udpPort := instanceTestUDPBackend(t, "127.0.0.1", 0, 'A')
				instanceTestUDPBackend(t, "127.0.0.2", udpPort, 'B')
				aPort := aKernel.nodeConfig.ServerPort
				instanceTestUDPFlow(t, aPort, udpPort, domain, 'A')
				bKernel, _ := start("127.0.0.2")
				instanceTestUDPFlow(t, bKernel.nodeConfig.ServerPort, udpPort, domain, 'B')
				instanceTestUDPFlow(t, aPort, udpPort, domain, 'A')
				bKernel.Stop()
				instanceTestUDPFlow(t, aPort, udpPort, domain, 'A')
				return
			}
			held, err := connect(a)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			if err := instanceTestMarker(held, 'A'); err != nil {
				t.Fatal("initial A connection", err)
			}
			// Keep opening fresh routes through A while B is initialized and closed.
			ready, stop, drained := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				err := fresh(a, 'A')
				close(ready)
				for err == nil {
					select {
					case <-stop:
						drained <- nil
						return
					default:
						err = fresh(a, 'A')
					}
				}
				drained <- err
			}()
			defer func() {
				close(stop)
				if err := <-drained; err != nil {
					t.Error("new A connection during B startup/shutdown", err)
				}
			}()
			<-ready
			bKernel, b := start("127.0.0.2")
			if err := fresh(b, 'B'); err != nil {
				t.Fatal("B's own outbound", err)
			}
			if err := fresh(a, 'A'); err != nil {
				t.Error("new A connection while B runs", err)
			}
			bKernel.Stop()
			if err := fresh(a, 'A'); err != nil {
				t.Error("new A connection after B stopped", err)
			}
			if err := instanceTestMarker(held, 'A'); err != nil {
				t.Error("original A connection", err)
			}
		})
	}
}
