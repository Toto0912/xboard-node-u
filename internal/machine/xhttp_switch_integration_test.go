package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/gorilla/websocket"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	"golang.org/x/net/proxy"
)

const switchTestUUID = "11111111-1111-1111-1111-111111111111"

func switchTestListen(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func switchTestWait(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Keep Xray's process-wide dialer separate from the node under test, as on a real client.
func TestMachineSwitchVLESSClientProcess(t *testing.T) {
	raw := os.Getenv("XBOARD_SWITCH_CLIENT_JSON")
	if raw == "" {
		t.Skip("helper process")
	}
	pb, err := serial.LoadJSONConfig(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	client, err := xcore.New(pb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, os.Stdin)
}

// Keep each authenticated connection open through the subsequent lifecycle events.
func switchTestStream(t *testing.T, port int, transport, echoAddr string) net.Conn {
	t.Helper()
	l := switchTestListen(t)
	socks := l.Addr().String()
	stream := `{"network":"tcp"}`
	if transport == "xhttp" {
		stream = `{"network":"xhttp","xhttpSettings":{"path":"/switch","mode":"packet-up"}}`
	}
	raw := fmt.Sprintf(`{"log":{"loglevel":"error"},"inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"socks","settings":{"auth":"noauth"}}],"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"127.0.0.1","port":%d,"users":[{"id":%q,"encryption":"none"}]}]},"streamSettings":%s}]}`, l.Addr().(*net.TCPAddr).Port, port, switchTestUUID, stream)
	l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMachineSwitchVLESSClientProcess$")
	cmd.Env = append(os.Environ(), "XBOARD_SWITCH_CLIENT_JSON="+raw, "GORACE=atexit_sleep_ms=0")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("Xray client process: %v", err)
		}
	})
	var conn net.Conn
	switchTestWait(t, "VLESS client listener", func() bool {
		conn, err = net.DialTimeout("tcp4", socks, time.Second)
		return err == nil
	})
	conn.Close()
	dialer, err := proxy.SOCKS5("tcp", socks, nil, &net.Dialer{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	conn, err = dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", echoAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func switchTestEcho(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != msg {
		t.Fatalf("%s: received %q: %v", msg, got, err)
	}
}

func TestMachineXHTTPTransportSwitchPreservesOtherConnections(t *testing.T) {
	for _, mode := range []string{"WebSocket", "REST", "WebSocket/XraySibling"} {
		t.Run(mode, func(t *testing.T) {
			overWS := mode != "REST"
			echo := switchTestListen(t)
			go func() {
				for {
					c, err := echo.Accept()
					if err != nil {
						return
					}
					go func() { defer c.Close(); io.Copy(c, c) }()
				}
			}()
			target, sibling := switchTestListen(t), switchTestListen(t)
			port := target.Addr().(*net.TCPAddr).Port
			siblingPort := sibling.Addr().(*net.TCPAddr).Port
			const loopbackRoute = `"custom_route_rules":[{"match":{"ip_cidrs":["127.0.0.1/32"]},"action":{"type":"direct"}}]`
			tcpConfig := func(port int) string {
				return fmt.Sprintf(`{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":"tcp",%s}`, port, loopbackRoute)
			}
			transport := "xhttp"
			if mode == "REST" {
				transport = "splithttp"
			}
			xhttpAt := func(port int, network string) string {
				return fmt.Sprintf(`{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":%q,"networkSettings":{"path":"/switch","mode":"packet-up"},%s,"custom_outbounds":[{"protocol":"freedom","tag":"direct","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.1/32"]}]}}]}`, port, network, loopbackRoute)
			}
			xhttpConfig := xhttpAt(port, transport)
			siblingConfig, siblingTransport := tcpConfig(siblingPort), "tcp"
			if mode == "WebSocket/XraySibling" {
				siblingConfig, siblingTransport = xhttpAt(siblingPort, "xhttp"), "xhttp"
			}
			var changed atomic.Bool
			var discoveries, targetUsers, targetConfigs atomic.Int32
			var targetTrafficReported atomic.Bool
			connected := make(chan *websocket.Conn, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/server/machine/nodes":
					discoveries.Add(1)
					io.WriteString(w, `{"nodes":[{"id":7,"type":"vless"},{"id":8,"type":"vless"}]}`)
				case "/api/v2/server/handshake":
					if overWS {
						fmt.Fprintf(w, `{"websocket":{"enabled":true,"ws_url":"ws://%s/ws"}}`, r.Host)
					} else {
						io.WriteString(w, `{"websocket":{"enabled":false}}`)
					}
				case "/ws":
					up := websocket.Upgrader{}
					conn, err := up.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					if err := conn.WriteJSON(map[string]string{"event": "auth.success"}); err != nil {
						return
					}
					connected <- conn
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				case "/api/v2/server/config":
					if r.URL.Query().Get("node_id") == "8" {
						io.WriteString(w, siblingConfig)
					} else if changed.Load() {
						targetConfigs.Add(1)
						io.WriteString(w, xhttpConfig)
					} else {
						io.WriteString(w, tcpConfig(port))
					}
				case "/api/v2/server/user":
					if r.URL.Query().Get("node_id") == "7" {
						targetUsers.Add(1)
					}
					fmt.Fprintf(w, `{"users":[{"id":1,"uuid":%q}]}`, switchTestUUID)
				case "/api/v2/server/report":
					var report struct {
						NodeID  int              `json:"node_id"`
						Traffic map[int][2]int64 `json:"traffic"`
					}
					if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					if bytes := report.Traffic[1]; report.NodeID == 7 && bytes[0] > 0 && bytes[1] > 0 {
						targetTrafficReported.Store(true)
					}
					io.WriteString(w, "{}")
				default:
					io.WriteString(w, "{}")
				}
			}))
			t.Cleanup(srv.Close)
			pull := 1
			if overWS {
				pull = 3600
			}
			o := New(&config.Config{
				Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "test"},
				Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir(), LogLevel: "error"},
				Node:   config.NodeConfig{TrackInterval: 60, PullInterval: pull, DeviceReportInterval: 60},
				WS:     config.WSConfig{DiscoveryInterval: 3600},
			})
			target.Close()
			sibling.Close()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- o.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("machine did not stop")
				}
			})
			waitListener := func(addr string) {
				t.Helper()
				switchTestWait(t, "listener "+addr, func() bool {
					c, err := net.DialTimeout("tcp4", addr, 150*time.Millisecond)
					if err != nil {
						return false
					}
					c.Close()
					return true
				})
			}
			waitListener(target.Addr().String())
			waitListener(sibling.Addr().String())
			sameHost := switchTestStream(t, siblingPort, siblingTransport, echo.Addr().String())
			switchTestEcho(t, sameHost, "sibling before switch")
			tcp := switchTestStream(t, port, "tcp", echo.Addr().String())
			switchTestEcho(t, tcp, "target before switch")
			o.mu.Lock()
			old := o.nodes[7]
			o.mu.Unlock()

			var ws *websocket.Conn
			push := func() {
				t.Helper()
				if err := ws.WriteJSON(map[string]any{"event": "sync.config", "data": map[string]any{"node_id": 7, "config": json.RawMessage(xhttpConfig)}}); err != nil {
					t.Fatal(err)
				}
			}
			if overWS {
				select {
				case ws = <-connected:
				case <-time.After(5 * time.Second):
					t.Fatal("machine did not connect to WS")
				}
				// Finish the initial reconnect pull before changing the panel.
				switchTestWait(t, "initial WS reconciliation", func() bool { return targetUsers.Load() >= 2 })
			}
			changed.Store(true)
			if ws != nil {
				push()
			}
			select {
			case <-old.done:
			case <-time.After(10 * time.Second):
				t.Fatal("machine did not replace target sing-box child")
			}
			if !targetTrafficReported.Load() {
				t.Fatal("old child stopped without reporting the existing flow's upload and download")
			}
			tcp.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, err := tcp.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("old TCP flow remained readable after sing-box stopped")
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("old TCP flow was not closed by the switch")
			}
			waitListener(target.Addr().String())
			xhttp := switchTestStream(t, port, "xhttp", echo.Addr().String())
			switchTestEcho(t, xhttp, "target after switch")
			switchTestEcho(t, sameHost, "sibling after switch")

			if ws != nil {
				for i := 0; i < 3; i++ {
					push()
				}
				// This ordered WS frame is a barrier after the duplicate frames.
				before := discoveries.Load()
				if err := ws.WriteJSON(map[string]any{"event": "sync.nodes", "data": map[string]any{"nodes": []any{}}}); err != nil {
					t.Fatal(err)
				}
				switchTestWait(t, "discovery after duplicate WS notices", func() bool { return discoveries.Load() > before })
			} else {
				before := targetConfigs.Load()
				switchTestWait(t, "next periodic config poll", func() bool { return targetConfigs.Load() > before })
			}
			switchTestEcho(t, xhttp, "target after duplicate config")
			switchTestEcho(t, sameHost, "sibling after duplicate config")
			if siblingTransport == "xhttp" {
				fresh := switchTestStream(t, siblingPort, "xhttp", echo.Addr().String())
				switchTestEcho(t, fresh, "fresh connection to the original Xray sibling")
			}
		})
	}
}
