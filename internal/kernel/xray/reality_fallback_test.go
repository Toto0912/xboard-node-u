package xray

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	goreality "github.com/xtls/reality"
	xnet "github.com/xtls/xray-core/common/net"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	xreality "github.com/xtls/xray-core/transport/internet/reality"
)

func TestStandaloneRealityNativeFallbacksAndAuthorizedProxy(t *testing.T) {
	backend := func(body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		t.Cleanup(s.Close)
		return s
	}
	defaultSite, pathSite, namedSite, alpnSite, proxySite := backend("native-default"), backend("native-path"), backend("native-named"), backend("native-alpn"), backend("authorized-proxy")
	cert := fallbackCertificate(t)
	pair, err := tls.X509KeyPair(cert.CertPEM, cert.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	camouflage := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "reality-original-target") }))
	camouflage.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1", "h2"}, CurvePreferences: []tls.CurveID{tls.X25519}}
	camouflage.StartTLS()
	defer camouflage.Close()
	target := strings.TrimPrefix(camouflage.URL, "https://")
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert.CertPEM)
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"vless", "trojan"} {
		t.Run(protocol, func(t *testing.T) {
			port := freeTestPort(t)
			flow := ""
			if protocol == "vless" {
				flow = "xtls-rprx-vision"
			}
			kcfg := config.KernelConfig{Type: "xray", LogLevel: "error", CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}}
			cfg := &config.Config{Kernel: kcfg, Standalone: &config.StandaloneConfig{Enabled: true,
				Node: config.StandaloneNodeConfig{Protocol: protocol, ListenIP: "127.0.0.1", ServerPort: port, Network: "raw", TLS: 2, Flow: flow,
					TLSSettings: map[string]any{"server_name": "localhost", "dest": target, "private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "short_id": "0123456789abcdef"},
					Fallbacks: []config.XrayFallback{
						{Dest: strings.TrimPrefix(defaultSite.URL, "http://")},
						{Path: "/special", Dest: strings.TrimPrefix(pathSite.URL, "http://")},
						{Name: "localhost", Path: "/named", Dest: strings.TrimPrefix(namedSite.URL, "http://")},
						{ALPN: "http/1.1", Path: "/alpn", Dest: strings.TrimPrefix(alpnSite.URL, "http://")},
						{Path: "/proxy-v1", Dest: fallbackProxyBackend(t, 1), Xver: 1},
						{Path: "/proxy-v2", Dest: fallbackProxyBackend(t, 2), Xver: 2},
					}}, Users: []config.StandaloneUser{{ID: 1, UUID: testUsers[0].UUID}}}}
			node, err := model.NodeSpecFromStandaloneValidated(cfg)
			if err != nil {
				t.Fatal(err)
			}
			node.CustomRoutes = []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}
			in := buildConfig(kcfg, node, testUsers, kernel.TLSCert{})["inbounds"].([]M)[0]
			stream := in["streamSettings"].(M)
			rs := stream["realitySettings"].(M)
			if rs["dest"] != target || rs["serverNames"].([]string)[0] != "localhost" || rs["privateKey"] != cfg.Standalone.Node.TLSSettings["private_key"] || rs["shortIds"].([]string)[0] != "0123456789abcdef" {
				t.Fatal("native fallbacks changed Reality handshake settings")
			}
			if _, exists := stream["tlsSettings"]; exists || len(in["settings"].(M)["fallbacks"].([]config.XrayFallback)) != 6 {
				t.Fatal("native fallbacks lost or ordinary TLS settings injected into Reality")
			}
			server := New(kcfg)
			if err := server.Start(node, testUsers, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Stop)
			waitRealityFallbackProbe(t, target)
			// A browser does not authenticate REALITY and must still reach its
			// original HTTPS target, never a plaintext protocol fallback.
			browser, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"http/1.1"}})
			if err != nil {
				t.Fatal(err)
			}
			checkFallbackHTTP(t, browser, "/", "reality-original-target")
			// This pinned REALITY implementation negotiates no ALPN. Preserve
			// nonempty ALPN rules, but do not force them to match HTTP/1.1.
			for _, tc := range []struct{ path, want string }{{"/", "native-default"}, {"/special", "native-path"}, {"/named", "native-named"}, {"/alpn", "native-default"}, {"/proxy-v1", "proxy-header-ok"}, {"/proxy-v2", "proxy-header-ok"}} {
				conn := authenticatedRealityFallbackConnection(t, port, key.PublicKey().Bytes())
				checkFallbackHTTP(t, conn, tc.path, tc.want)
			}
			settings := M{}
			if protocol == "vless" {
				settings["vnext"] = []M{{"address": "127.0.0.1", "port": port, "users": []M{{"id": testUsers[0].UUID, "encryption": "none", "flow": flow}}}}
			} else {
				settings["servers"] = []M{{"address": "127.0.0.1", "port": port, "password": testUsers[0].UUID}}
			}
			raw, _ := json.Marshal(M{"log": M{"loglevel": "error"}, "outbounds": []M{{"protocol": protocol, "settings": settings, "streamSettings": M{"network": "raw", "security": "reality", "realitySettings": M{"serverName": "localhost", "fingerprint": "chrome", "password": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "shortId": "0123456789abcdef"}}}}})
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
			dest, _ := xnet.ParseDestination("tcp:" + strings.TrimPrefix(proxySite.URL, "http://"))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, err := xcore.Dial(ctx, client, dest)
			if err != nil {
				t.Fatal(err)
			}
			checkFallbackHTTP(t, conn, "/", "authorized-proxy")
		})
	}
}

func waitRealityFallbackProbe(t *testing.T, target string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, _ := goreality.GlobalPostHandshakeRecordsLens.Load(target + " localhost 2")
		if _, ready := value.([]int); ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Reality target probe did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func authenticatedRealityFallbackConnection(t *testing.T, port int, publicKey []byte) net.Conn {
	t.Helper()
	dest := xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(port))
	conn, err := net.DialTimeout("tcp", dest.NetAddr(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secured, err := xreality.UClient(conn, &xreality.Config{ServerName: "localhost", Fingerprint: "chrome", PublicKey: publicKey, ShortId: []byte{1, 35, 69, 103, 137, 171, 205, 239}, SpiderX: "/", SpiderY: make([]int64, 10)}, ctx, dest)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	return secured
}
