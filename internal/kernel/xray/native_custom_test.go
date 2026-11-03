package xray

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A loopback SOCKS5 upstream proves the generated core actually selects the
// supplied outbound. The requested .invalid domain cannot succeed via direct.
func TestNativeCustomOutboundRealTraffic(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-%v", legacy), func(t *testing.T) {
			upstream, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer upstream.Close()
			handled := make(chan error, 1)
			go func() {
				conn, err := upstream.Accept()
				if err != nil {
					handled <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(8 * time.Second))
				header := make([]byte, 2)
				if _, err = io.ReadFull(conn, header); err != nil {
					handled <- err
					return
				}
				methods := make([]byte, int(header[1]))
				if _, err = io.ReadFull(conn, methods); err != nil {
					handled <- err
					return
				}
				conn.Write([]byte{5, 0})
				request := make([]byte, 4)
				if _, err = io.ReadFull(conn, request); err != nil {
					handled <- err
					return
				}
				if request[0] != 5 || request[1] != 1 || request[3] != 3 {
					handled <- fmt.Errorf("unexpected SOCKS request %v", request)
					return
				}
				length := make([]byte, 1)
				io.ReadFull(conn, length)
				target := make([]byte, int(length[0])+2)
				io.ReadFull(conn, target)
				if string(target[:len(target)-2]) != "native-custom.invalid" {
					handled <- fmt.Errorf("wrong destination %q", target)
					return
				}
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
				payload := make([]byte, 4)
				if _, err = io.ReadFull(conn, payload); err != nil {
					handled <- err
					return
				}
				if string(payload) != "ping" {
					handled <- fmt.Errorf("wrong payload")
					return
				}
				_, err = io.WriteString(conn, "native-socks-ok")
				handled <- err
			}()
			port := upstream.Addr().(*net.TCPAddr).Port
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			entryPort := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			outbound := fmt.Sprintf(`{"protocol":"socks","tag":"proxy","settings":{"address":"127.0.0.1","port":%d},"streamSettings":{"sockopt":{"tcpNoDelay":true}},"mux":{"enabled":false}}`, port)
			if legacy {
				outbound = fmt.Sprintf(`{"protocol":"socks","tag":"proxy","settings":{"servers":[{"address":"127.0.0.1","port":%d}]}}`, port)
			}
			raw := fmt.Sprintf(`{"protocol":"vless","listen_ip":"127.0.0.1","server_port":%d,"network":"tcp","custom_outbounds":[%s],"custom_routes":[{"localPort":"%d","outboundTag":"proxy"}]}`, entryPort, outbound, entryPort)
			var nc panel.NodeConfig
			if err = json.Unmarshal([]byte(raw), &nc); err != nil {
				t.Fatal(err)
			}
			kcfg := config.KernelConfig{Type: "xray", LogLevel: "error", ConfigDir: t.TempDir()}
			node, err := model.NodeSpecFromPanelValidated(&nc, kcfg)
			if err != nil {
				t.Fatal(err)
			}
			generated := buildConfig(kcfg, node, testUsers, kernel.TLSCert{})
			for _, item := range generated["outbounds"].([]M) {
				if item["tag"] == "proxy" {
					if !legacy {
						if _, ok := item["streamSettings"]; !ok {
							t.Fatal("lost native streamSettings")
						}
						if _, ok := item["mux"]; !ok {
							t.Fatal("lost native mux")
						}
					}
				}
			}
			core := New(kcfg)
			if err = core.Start(node, testUsers, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			defer core.Stop()
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", entryPort), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(8 * time.Second))
			uuid, err := hex.DecodeString(strings.ReplaceAll(testUsers[0].UUID, "-", ""))
			if err != nil {
				t.Fatal(err)
			}
			request := append([]byte{0}, uuid...)
			request = append(request, 0, 1, 0, 80, 2, byte(len("native-custom.invalid")))
			request = append(request, []byte("native-custom.invalid")...)
			request = append(request, []byte("ping")...)
			if _, err = conn.Write(request); err != nil {
				t.Fatal(err)
			}
			response := make([]byte, 2)
			if _, err = io.ReadFull(conn, response); err != nil {
				t.Fatal(err)
			}
			if response[1] > 0 {
				io.CopyN(io.Discard, conn, int64(response[1]))
			}
			result := make([]byte, len("native-socks-ok"))
			if _, err = io.ReadFull(conn, result); err != nil {
				t.Fatal(err)
			}
			if string(result) != "native-socks-ok" {
				t.Fatalf("wrong response %q", result)
			}
			if err = <-handled; err != nil {
				t.Fatal(err)
			}
		})
	}
}
