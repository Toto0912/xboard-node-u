//go:build with_quic

package kernel_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	sb "github.com/cedar2025/xboard-node/internal/kernel/singbox"
	xr "github.com/cedar2025/xboard-node/internal/kernel/xray"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/sagernet/quic-go/http3"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	J "github.com/sagernet/sing/common/json"
	M "github.com/sagernet/sing/common/metadata"
)

func hy2Cert(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, e := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	k, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	return kernel.TLSCert{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})}
}

func TestHY2RealMasqueradeAuthenticationAndTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Proxy-Host", r.Host)
		w.Header().Set("X-Forwarded-Seen", r.Header.Get("X-Forwarded-For"))
		io.WriteString(w, "proxy-response")
	}))
	defer upstream.Close()
	echo, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	go func() {
		for {
			c, e := echo.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	cert := hy2Cert(t)
	for _, core := range []string{"singbox", "xray"} {
		for _, mode := range []string{"default", "string", "proxy", "file", "brutal", "upload-only", "download-only", "client-bbr", "obfs", "gecko", "legacy", "legacy-bandwidth"} {
			t.Run(core+"/"+mode, func(t *testing.T) {
				l, e := net.ListenPacket("udp", "127.0.0.1:0")
				if e != nil {
					t.Fatal(e)
				}
				port := l.LocalAddr().(*net.UDPAddr).Port
				l.Close()
				n := &model.NodeSpec{Protocol: "hysteria", Version: 2, ListenIP: "127.0.0.1", ServerPort: port, HY2Settings: &config.HY2Settings{BandwidthMode: "auto", Kernel: core}}
				if core == "singbox" {
					n.CustomRoutes = []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "action": "route", "outbound": "direct"}}
				} else {
					n.CustomRoutes = []map[string]any{{"ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}
				}
				want := "404 page not found\n"
				status := 404
				if mode == "string" {
					key := "status_code"
					if core == "xray" {
						key = "statusCode"
					}
					n.HY2Settings.Masquerade = map[string]any{"type": "string", key: 201, "content": "string-response", "headers": map[string]any{"X-Example": "yes"}}
					want = "string-response"
					status = 201
				}
				if mode == "proxy" {
					n.HY2Settings.Masquerade = map[string]any{"type": "proxy", "url": upstream.URL}
					key := "rewrite_host"
					if core == "xray" {
						key = "rewriteHost"
						n.HY2Settings.Masquerade["xForwarded"] = true
					}
					n.HY2Settings.Masquerade[key] = true
					want = "proxy-response"
					status = 200
				}
				if mode == "file" {
					dir := t.TempDir()
					if e := os.WriteFile(filepath.Join(dir, "index.html"), []byte("file-response"), 0600); e != nil {
						t.Fatal(e)
					}
					key := "directory"
					if core == "xray" {
						key = "dir"
					}
					n.HY2Settings.Masquerade = map[string]any{"type": "file", key: dir}
					want = "file-response"
					status = 200
				}
				if mode == "brutal" {
					n.UpMbps = 47
					n.DownMbps = 23
				}
				if mode == "upload-only" {
					n.UpMbps = 47
				}
				if mode == "download-only" {
					n.DownMbps = 23
				}
				if mode == "client-bbr" || mode == "legacy-bandwidth" {
					n.UpMbps, n.DownMbps = 47, 23
				}
				if mode == "obfs" || mode == "gecko" {
					n.Obfs = "salamander"
					if mode == "gecko" {
						n.Obfs = "gecko"
					}
					n.ObfsPassword = "obfs-test-password"
				}
				if mode == "legacy" || mode == "legacy-bandwidth" {
					n.HY2Settings = nil
				}
				if e := model.ValidateNodeSpec(n, config.KernelConfig{Type: core}); e != nil {
					t.Fatal(e)
				}
				var server kernel.Kernel
				if core == "singbox" {
					server = sb.New(config.KernelConfig{Type: core, LogLevel: "error", ConfigDir: t.TempDir()})
				} else {
					server = xr.New(config.KernelConfig{Type: core, LogLevel: "error", ConfigDir: t.TempDir(), CustomOutbound: []map[string]any{{"protocol": "freedom", "tag": "direct", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}})
				}
				defer server.Stop()
				users := []model.UserSpec{{ID: 1, UUID: "hy2-integration-password"}}
				if e := server.Start(n, users, cert); e != nil {
					t.Fatal(e)
				}
				if mode != "obfs" && mode != "gecko" {
					tr := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}}
					defer tr.Close()
					client := &http.Client{Transport: tr, Timeout: 4 * time.Second}
					address := fmt.Sprintf("https://127.0.0.1:%d/", port)
					r, e := client.Get(address)
					if e != nil {
						t.Fatal(e)
					}
					body, _ := io.ReadAll(r.Body)
					r.Body.Close()
					if r.StatusCode != status || string(body) != want {
						t.Fatalf("masquerade %d %q, want %d %q", r.StatusCode, body, status, want)
					}
					if mode == "string" && r.Header.Get("X-Example") != "yes" {
						t.Fatal("custom response headers were ignored")
					}
					if mode == "proxy" && core == "xray" && r.Header.Get("X-Forwarded-Seen") == "" {
						t.Fatal("xForwarded was ignored")
					}
					// Incorrect auth must be indistinguishable from the configured website.
					req, _ := http.NewRequest("POST", address+"auth", nil)
					req.Host = "hysteria"
					req.Header.Set("Hysteria-Auth", "wrong-password")
					r, e = client.Do(req)
					if e != nil {
						t.Fatal(e)
					}
					r.Body.Close()
					if r.StatusCode == 233 {
						t.Fatal("bad auth accepted")
					}
					req, _ = http.NewRequest("POST", address+"auth", nil)
					req.Host = "hysteria"
					req.Header.Set("Hysteria-Auth", users[0].UUID)
					req.Header.Set("Hysteria-CC-RX", "100000000")
					r, e = client.Do(req)
					if e != nil {
						t.Fatal(e)
					}
					r.Body.Close()
					if r.StatusCode != 233 {
						t.Fatalf("authentication %d", r.StatusCode)
					}
					if n.HY2Settings != nil {
						wantRate := "auto"
						if n.UpMbps > 0 || n.DownMbps > 0 {
							wantRate = strconv.Itoa(n.DownMbps * 1000000 / 8)
						}
						if r.Header.Get("Hysteria-CC-RX") != wantRate {
							t.Fatalf("negotiation got %q want %q", r.Header.Get("Hysteria-CC-RX"), wantRate)
						}
						repeated, err := client.Do(req.Clone(context.Background()))
						if err != nil {
							t.Fatal(err)
						}
						repeated.Body.Close()
						if repeated.StatusCode != 233 || repeated.Header.Get("Hysteria-CC-RX") != wantRate {
							t.Fatal("repeated authentication changed bandwidth policy")
						}
					}
				}
				out := map[string]any{"type": "hysteria2", "tag": "proxy", "server": "127.0.0.1", "server_port": port, "password": users[0].UUID, "up_mbps": 100, "down_mbps": 100, "tls": map[string]any{"enabled": true, "insecure": true, "server_name": "localhost"}}
				if n.Obfs != "" {
					out["obfs"] = map[string]any{"type": n.Obfs, "password": n.ObfsPassword}
				}
				if mode == "client-bbr" {
					delete(out, "up_mbps")
					delete(out, "down_mbps")
				}
				bytes, _ := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "outbounds": []any{out}})
				ctx := include.Context(context.Background())
				opts, e := J.UnmarshalExtendedContext[option.Options](ctx, bytes)
				if e != nil {
					t.Fatal(e)
				}
				client, e := box.New(box.Options{Context: ctx, Options: opts})
				if e != nil {
					t.Fatal(e)
				}
				defer client.Close()
				if e := client.Start(); e != nil {
					t.Fatal(e)
				}
				o, _ := client.Outbound().Outbound("proxy")
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				conn, e := o.DialContext(ctx, "tcp", M.ParseSocksaddr(echo.Addr().String()))
				if e != nil {
					t.Fatal(e)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(4 * time.Second))
				payload := "hy2-real-proxy-payload"
				if _, e = io.WriteString(conn, payload); e != nil {
					t.Fatal(e)
				}
				got := make([]byte, len(payload))
				if _, e = io.ReadFull(conn, got); e != nil || string(got) != payload {
					t.Fatalf("proxy echo %q: %v", got, e)
				}
				if mode == "default" {
					before := kernel.ComputeHash(n, users)
					n.HY2Settings.Masquerade = map[string]any{"type": "string", "content": "reloaded"}
					if before == kernel.ComputeHash(n, users) {
						t.Fatal("masquerade changes must trigger reload")
					}
					if err := server.Reload(n, users, cert); err != nil {
						t.Fatal(err)
					}
					tr := &http3.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "localhost"}}
					defer tr.Close()
					r, err := (&http.Client{Transport: tr, Timeout: 4 * time.Second}).Get(fmt.Sprintf("https://127.0.0.1:%d/", port))
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(r.Body)
					r.Body.Close()
					if string(body) != "reloaded" {
						t.Fatalf("masquerade reload did not apply: %q", body)
					}
				}
			})
		}
	}
}
