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
	"github.com/cedar2025/xboard-node/internal/panel"
	goreality "github.com/xtls/reality"
	xnet "github.com/xtls/xray-core/common/net"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
)

func TestPanelFallbackRealTLSRealityAndAuthorizedProxy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "panel-http-site") }))
	defer site.Close()
	cert := fallbackCertificate(t)
	pair, err := tls.X509KeyPair(cert.CertPEM, cert.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	httpsSite := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "panel-https-site") }))
	httpsSite.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2", "http/1.1"}, CurvePreferences: []tls.CurveID{tls.X25519}}
	httpsSite.StartTLS()
	defer httpsSite.Close()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert.CertPEM)
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"vless", "trojan"} {
		for _, security := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s-tls%d", protocol, security), func(t *testing.T) {
				target := strings.TrimPrefix(site.URL, "http://")
				want := "panel-http-site"
				if security == 2 {
					target = strings.TrimPrefix(httpsSite.URL, "https://")
					want = "panel-https-site"
				}
				port := freeTestPort(t)
				pc := &panel.NodeConfig{Protocol: protocol, ListenIP: "127.0.0.1", ServerPort: port, Network: "tcp", TLS: security,
					XrayFallback: panel.OptionalFallbackTarget(target), TLSSettings: map[string]any{"server_name": "localhost", "private_key": base64.RawURLEncoding.EncodeToString(key.Bytes()), "short_id": "0123456789abcdef", "dest": "127.0.0.1:1"}}
				kcfg := config.KernelConfig{Type: "xray", LogLevel: "error", CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}}
				node, err := model.NodeSpecFromPanelValidated(pc, kcfg)
				if err != nil {
					t.Fatal(err)
				}
				node.CustomRoutes = []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}
				rendered := buildConfig(kcfg, node, testUsers, cert)
				in := rendered["inbounds"].([]M)[0]
				stream := in["streamSettings"].(M)
				if security == 2 {
					rs := stream["realitySettings"].(M)
					if rs["dest"] != target || rs["serverNames"].([]string)[0] != "localhost" {
						t.Fatalf("lost target or SNI: %+v", rs)
					}
					if _, ok := in["settings"].(M)["fallbacks"]; ok {
						t.Fatal("Reality acquired plaintext fallback")
					}
				}
				server := New(kcfg)
				if err := server.Start(node, testUsers, cert); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(server.Stop)
				if security == 2 {
					// RAW starts REALITY's target probe asynchronously. Wait for its
					// cache before testing the first authenticated connection; an
					// unfinished probe deliberately stalls a handshake for five seconds.
					deadline := time.Now().Add(3 * time.Second)
					for {
						value, _ := goreality.GlobalPostHandshakeRecordsLens.Load(target + " localhost 2")
						if _, ready := value.([]int); ready {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("Reality target probe did not finish")
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: roots, ServerName: "localhost", NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS13})
				if err != nil {
					t.Fatal(err)
				}
				checkFallbackHTTP(t, conn, "/", want)
				settings := M{}
				if protocol == "vless" {
					settings["vnext"] = []M{{"address": "127.0.0.1", "port": port, "users": []M{{"id": testUsers[0].UUID, "encryption": "none"}}}}
				} else {
					settings["servers"] = []M{{"address": "127.0.0.1", "port": port, "password": testUsers[0].UUID}}
				}
				transport := M{"network": "tcp", "security": "tls", "tlsSettings": M{"serverName": "localhost", "alpn": []string{"http/1.1"}, "pinnedPeerCertSha256": certPin(cert)}}
				if security == 2 {
					transport = M{"network": "tcp", "security": "reality", "realitySettings": M{"serverName": "localhost", "fingerprint": "chrome", "password": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()), "shortId": "0123456789abcdef", "spiderX": "/"}}
				}
				raw, _ := json.Marshal(M{"log": M{"loglevel": "error"}, "outbounds": []M{{"protocol": protocol, "settings": settings, "streamSettings": transport}}})
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
				dest, err := xnet.ParseDestination("tcp:" + strings.TrimPrefix(site.URL, "http://"))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				proxyConn, err := xcore.Dial(ctx, client, dest)
				if err != nil {
					t.Fatal(err)
				}
				checkFallbackHTTP(t, proxyConn, "/", "panel-http-site")
			})
		}
	}
}

func TestPanelFallbackAbsentOrUnsupportedPreservesNativeConfig(t *testing.T) {
	for _, network := range []string{"tcp", "raw", "ws", "grpc", "xhttp"} {
		node := &model.NodeSpec{Protocol: "vless", TLS: 1, Network: network, ServerPort: 18443}
		before, _ := json.Marshal(buildConfig(config.KernelConfig{}, node, testUsers, kernel.TLSCert{}))
		if network == "tcp" || network == "raw" {
			node.XrayFallback = "invalid"
		} else {
			node.XrayFallback = "127.0.0.1:8080"
		}
		after, _ := json.Marshal(buildConfig(config.KernelConfig{}, node, testUsers, kernel.TLSCert{}))
		if !bytes.Equal(before, after) {
			t.Fatalf("invalid or unsupported setting changed %s config", network)
		}
	}
}
