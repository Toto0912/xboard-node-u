package xray

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	"github.com/cedar2025/xboard-node/internal/panel"
	xnet "github.com/xtls/xray-core/common/net"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
)

func TestStandaloneFallbackNativeConfigAndPanelIsolation(t *testing.T) {
	customPath := filepath.Join(t.TempDir(), "custom.json")
	if err := os.WriteFile(customPath, []byte(`{"inbounds":[{"settings":{"fallbacks":[{"dest":9999}]}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"vless", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			kcfg := config.KernelConfig{Type: "xray", CustomConfig: customPath}
			local := &config.Config{Kernel: kcfg, Standalone: &config.StandaloneConfig{Enabled: true,
				Node: config.StandaloneNodeConfig{Protocol: protocol, ServerPort: 8443, TLS: 1,
					Fallbacks:   []config.XrayFallback{{Dest: 8080}, {Name: "localhost", ALPN: "h2", Dest: "/tmp/site.sock", Xver: 2}},
					TLSSettings: map[string]any{"alpn": []string{"h2", "http/1.1"}}}}}
			node, err := model.NodeSpecFromStandaloneValidated(local)
			if err != nil {
				t.Fatal(err)
			}
			data, err := marshalConfig(kcfg, node, testUsers, upgradeCertificate(t))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := serial.LoadJSONConfig(bytes.NewReader(data)); err != nil {
				t.Fatalf("native Xray rejected fallback config: %v", err)
			}
			var rendered map[string]any
			if err := json.Unmarshal(data, &rendered); err != nil {
				t.Fatal(err)
			}
			inbound := rendered["inbounds"].([]any)[0].(map[string]any)
			rules := inbound["settings"].(map[string]any)["fallbacks"].([]any)
			if len(rules) != 2 || rules[0].(map[string]any)["dest"] != float64(8080) {
				t.Fatal("custom config replaced or lost standalone fallbacks")
			}
			remote := model.NodeSpecFromPanel(&panel.NodeConfig{Protocol: protocol, TLS: 1, ServerPort: 8443,
				TLSSettings: map[string]any{"alpn": []any{"h2"}, "fallbacks": []any{map[string]any{"dest": 8080}}}})
			cfg := buildConfig(kcfg, remote, testUsers, kernel.TLSCert{})
			in := cfg["inbounds"].([]M)[0]
			if _, ok := in["settings"].(M)["fallbacks"]; ok {
				t.Fatal("panel or machine inbound acquired fallbacks")
			}
			if _, ok := in["streamSettings"].(M)["tlsSettings"].(M)["alpn"]; ok {
				t.Fatal("panel or machine TLS behavior changed")
			}
		})
	}
}

func TestStandaloneFallbackRealTLSAndProxy(t *testing.T) {
	backend := func(body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		t.Cleanup(s.Close)
		return s
	}
	defaultSite, pathSite, alpnSite, sniSite, proxySite := backend("default-site"), backend("path-site"), backend("alpn-site"), backend("sni-site"), backend("proxy-ok")
	for _, mode := range []struct{ name, protocol, flow string }{
		{"vless", "vless", ""},
		{"vless-vision", "vless", "xtls-rprx-vision"},
		{"trojan", "trojan", ""},
	} {
		t.Run(mode.name, func(t *testing.T) {
			protocol := mode.protocol
			cfgPath := filepath.Join(t.TempDir(), "config.yml")
			port := freeTestPort(t)
			proxyV1, proxyV2 := fallbackProxyBackend(t, 1), fallbackProxyBackend(t, 2)
			text := fmt.Sprintf(`kernel:
  type: xray
standalone:
  enabled: true
  node:
    protocol: %s
    listen_ip: "127.0.0.1"
    server_port: %d
    network: tcp
    tls: 1
    flow: %q
    tls_settings:
      server_name: localhost
      alpn: [fallback-test, http/1.1]
    fallbacks:
      - dest: "%s"
      - path: /special
        dest: "%s"
      - alpn: fallback-test
        dest: "%s"
      - name: other.localhost
        dest: "%s"
      - path: /proxy-v1
        dest: "%s"
        xver: 1
      - path: /proxy-v2
        dest: "%s"
        xver: 2
  users:
    - id: 1
      uuid: "279d4f89-3a2c-488d-a67c-2d39a72acdde"
health_port: 0
`, protocol, port, mode.flow, strings.TrimPrefix(defaultSite.URL, "http://"), strings.TrimPrefix(pathSite.URL, "http://"), strings.TrimPrefix(alpnSite.URL, "http://"), strings.TrimPrefix(sniSite.URL, "http://"), proxyV1, proxyV2)
			if err := os.WriteFile(cfgPath, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			node, err := model.NodeSpecFromStandaloneValidated(cfg)
			if err != nil {
				t.Fatal(err)
			}
			// Only this isolated proxy test allows loopback through the router
			// and freedom's private-address guard. Production defaults stay intact.
			node.CustomRoutes = []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}
			cfg.Kernel.CustomOutbound = []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{
				"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}},
			}}}
			cert := fallbackCertificate(t)
			server := New(cfg.Kernel)
			if err := server.Start(node, model.UserSpecsFromStandalone(cfg), cert); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Stop)
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(cert.CertPEM) {
				t.Fatal("invalid test certificate")
			}
			for _, tc := range []struct{ path, alpn, sni, want string }{
				{"/", "http/1.1", "localhost", "default-site"},
				{"/special", "http/1.1", "localhost", "path-site"},
				{"/", "fallback-test", "localhost", "alpn-site"},
				{"/", "http/1.1", "other.localhost", "sni-site"},
				{"/proxy-v1", "http/1.1", "localhost", "proxy-header-ok"},
				{"/proxy-v2", "http/1.1", "localhost", "proxy-header-ok"},
			} {
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port),
					&tls.Config{RootCAs: roots, ServerName: tc.sni, NextProtos: []string{tc.alpn}, MinVersion: tls.VersionTLS12})
				if err != nil {
					t.Fatal(err)
				}
				checkFallbackHTTP(t, conn, tc.path, tc.want)
			}

			settings := M{}
			if protocol == "vless" {
				settings["vnext"] = []M{{"address": "127.0.0.1", "port": port,
					"users": []M{{"id": cfg.Standalone.Users[0].UUID, "encryption": "none", "flow": mode.flow}}}}
			} else {
				settings["servers"] = []M{{"address": "127.0.0.1", "port": port, "password": cfg.Standalone.Users[0].UUID}}
			}
			raw, _ := json.Marshal(M{"log": M{"loglevel": "error"}, "outbounds": []M{{"protocol": protocol, "settings": settings,
				"streamSettings": M{"network": "tcp", "security": "tls", "tlsSettings": M{
					"serverName": "localhost", "alpn": []string{"http/1.1"}, "pinnedPeerCertSha256": certPin(cert)}}}}})
			pb, err := serial.LoadJSONConfig(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			client, err := xcore.New(pb)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			dest, err := xnet.ParseDestination("tcp:" + strings.TrimPrefix(proxySite.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, err := xcore.Dial(ctx, client, dest)
			if err != nil {
				t.Fatal(err)
			}
			checkFallbackHTTP(t, conn, "/", "proxy-ok")
		})
	}
}

func fallbackCertificate(t *testing.T) kernel.TLSCert {
	t.Helper()
	cert := upgradeCertificate(t)
	block, _ := pem.Decode(cert.CertPEM)
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	block, _ = pem.Decode(cert.KeyPEM)
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	parsed.DNSNames = []string{"localhost", "other.localhost"}
	signer := key.(crypto.Signer)
	der, err := x509.CreateCertificate(rand.Reader, parsed, parsed, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	cert.CertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert
}

func fallbackProxyBackend(t *testing.T, version int) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(conn)
		if version == 1 {
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "PROXY TCP4 127.0.0.1 ") || !strings.HasSuffix(line, "\r\n") {
				return
			}
		} else {
			header := make([]byte, 16)
			if _, err := io.ReadFull(reader, header); err != nil || string(header[:12]) != "\r\n\r\n\x00\r\nQUIT\n" || header[12] != 0x21 {
				return
			}
			length := int(header[14])<<8 | int(header[15])
			if length < 12 {
				return
			}
			if _, err := io.CopyN(io.Discard, reader, int64(length)); err != nil {
				return
			}
		}
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_ = request.Body.Close()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 15\r\nConnection: close\r\n\r\nproxy-header-ok")
	}()
	return listener.Addr().String()
}

func checkFallbackHTTP(t *testing.T, conn net.Conn, path, want string) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", path); err != nil {
		t.Fatalf("write HTTP request: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read HTTP response: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != want {
		t.Fatalf("HTTP result: status=%d body=%q error=%v, want %q", response.StatusCode, body, err, want)
	}
}
