package xray

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	xnet "github.com/xtls/xray-core/common/net"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func upgradeCertificate(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return kernel.TLSCert{CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}
}

func freeTestPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func TestUpgradeRealProtocols(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	for _, name := range []string{"vmess", "vless", "trojan", "shadowsocks", "ss2022", "vless-ws", "vless-grpc", "vless-xhttp", "vless-mux", "hysteria"} {
		t.Run(name, func(t *testing.T) {
			n := &model.NodeSpec{Protocol: name, ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), Cipher: "aes-128-gcm", CustomRoutes: []map[string]any{{"type": "field", "ip": []string{"127.0.0.1/32"}, "outboundTag": "direct"}}}
			switch name {
			case "hysteria":
				n.Version = 2
			case "vless-mux":
				n.Protocol = "vless"
			case "vless-ws":
				n.Protocol = "vless"
				n.Network = "ws"
				n.NetworkSettings = map[string]any{"path": "/test"}
			case "vless-grpc":
				n.Protocol = "vless"
				n.Network = "grpc"
				n.NetworkSettings = map[string]any{"serviceName": "test"}
			case "vless-xhttp":
				n.Protocol = "vless"
				n.Network = "xhttp"
				n.NetworkSettings = map[string]any{"path": "/test", "mode": "packet-up"}
			case "ss2022":
				n.Protocol = "shadowsocks"
				n.Cipher = "2022-blake3-aes-128-gcm"
				n.ServerKey = base64.StdEncoding.EncodeToString([]byte("server-key-16byt"))
			}
			cert := kernel.TLSCert{}
			if name == "trojan" || name == "hysteria" {
				n.TLS = 1
				cert = upgradeCertificate(t)
			}
			s := New(config.KernelConfig{Type: "xray", LogLevel: "error", CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "ip": []string{"127.0.0.1/32"}}}}}}})
			defer s.Stop()
			if err := s.Start(n, testUsers[:1], cert); err != nil {
				t.Fatal(err)
			}
			original := s.instance
			dial := func(user model.UserSpec) net.Conn {
				t.Helper()
				settings := map[string]any{}
				account := map[string]any{"id": user.UUID, "encryption": "none", "security": "auto"}
				switch n.Protocol {
				case "hysteria":
					settings["version"] = 2
					settings["address"] = "127.0.0.1"
					settings["port"] = n.ServerPort
				case "vless", "vmess":
					settings["vnext"] = []any{map[string]any{"address": "127.0.0.1", "port": n.ServerPort, "users": []any{account}}}
				case "trojan":
					settings["servers"] = []any{map[string]any{"address": "127.0.0.1", "port": n.ServerPort, "password": user.UUID}}
				case "shadowsocks":
					password := user.UUID
					if name == "ss2022" {
						key := make([]byte, 16)
						copy(key, user.UUID)
						password = n.ServerKey + ":" + base64.StdEncoding.EncodeToString(key)
					}
					settings["servers"] = []any{map[string]any{"address": "127.0.0.1", "port": n.ServerPort, "method": n.Cipher, "password": password}}
				}
				stream := map[string]any{"network": "tcp"}
				if name == "hysteria" {
					stream["network"] = "hysteria"
					stream["hysteriaSettings"] = map[string]any{"version": 2, "auth": user.UUID}
				}
				if n.Network != "" {
					stream["network"] = n.Network
					switch n.Network {
					case "ws":
						stream["wsSettings"] = map[string]any{"path": "/test"}
					case "grpc":
						stream["grpcSettings"] = map[string]any{"serviceName": "test"}
					case "xhttp":
						stream["xhttpSettings"] = map[string]any{"path": "/test", "mode": "packet-up"}
					}
				}
				if n.TLS == 1 {
					stream["security"] = "tls"
					stream["tlsSettings"] = map[string]any{"serverName": "localhost", "pinnedPeerCertSha256": certPin(cert)}
				}
				if name == "hysteria" {
					stream["tlsSettings"].(map[string]any)["alpn"] = []string{"h3"}
				}
				outbound := map[string]any{"protocol": n.Protocol, "settings": settings, "streamSettings": stream}
				if name == "vless-mux" {
					outbound["mux"] = map[string]any{"enabled": true, "concurrency": 4}
				}
				raw, _ := json.Marshal(map[string]any{"log": map[string]any{"loglevel": "error"}, "outbounds": []any{outbound}})
				pb, err := serial.LoadJSONConfig(bytes.NewReader(raw))
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
				dest, err := xnet.ParseDestination("tcp:" + echo.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				t.Cleanup(cancel)
				c, err := xcore.Dial(ctx, client, dest)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}
			echoRequest := func(c net.Conn) {
				t.Helper()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				payload := []byte("xray-upgrade-real-connection")
				if _, err := c.Write(payload); err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(payload))
				if _, err := io.ReadFull(c, got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, payload) {
					t.Fatal("payload mismatch")
				}
			}
			first := dial(testUsers[0])
			echoRequest(first)
			if _, err := s.AddUsers(testUsers[1:]); err != nil {
				t.Fatal(err)
			}
			if s.instance != original {
				t.Fatal("user update restarted kernel")
			}
			echoRequest(first)
			second := dial(testUsers[1])
			echoRequest(second)
			if _, err := s.RemoveUsers(testUsers[:1]); err != nil {
				t.Fatal(err)
			}
			echoRequest(second)
			traffic, _, _, err := s.GetUserTraffic(context.Background())
			if err != nil || traffic[testUsers[1].ID][0] == 0 || traffic[testUsers[1].ID][1] == 0 {
				t.Fatalf("missing traffic: %v %v", traffic, err)
			}
			first.Close()
			second.Close()
		})
	}
}

func certPin(cert kernel.TLSCert) string {
	block, _ := pem.Decode(cert.CertPEM)
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])
}
