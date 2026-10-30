package singbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
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

var upgradeProtocols = []string{"vmess", "vless", "shadowsocks", "socks", "http", "trojan", "anytls"}

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

// Exercise the real kernel and real protocol authentication, not just JSON syntax.
func TestUpgradeLiveUsers(t *testing.T) {
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
	for _, proto := range upgradeProtocols {
		t.Run(proto, func(t *testing.T) {
			n := &model.NodeSpec{Protocol: proto, ListenIP: "127.0.0.1", ServerPort: freeTestPort(t), Cipher: "aes-128-gcm",
				CustomRoutes: []map[string]any{{"ip_cidr": []string{"127.0.0.1/32"}, "outbound": "direct"}}}
			cert := kernel.TLSCert{}
			if proto == "trojan" || proto == "anytls" || proto == "hysteria2" || proto == "tuic" {
				n.TLS = 1
				cert = upgradeCertificate(t)
			}
			if proto == "hysteria2" {
				n.Protocol = "hysteria"
				n.Version = 2
			}
			s := New(config.KernelConfig{LogLevel: "error"})
			defer s.Stop()
			if err := s.Start(n, testUsers[:1], cert); err != nil {
				t.Fatal(err)
			}
			original := s.box
			dial := func(user model.UserSpec) net.Conn {
				t.Helper()
				outbound := map[string]any{"type": proto, "tag": "proxy", "server": "127.0.0.1", "server_port": n.ServerPort}
				if cert.HasCert() {
					outbound["tls"] = map[string]any{"enabled": true, "insecure": true, "server_name": "localhost", "alpn": []string{"h3"}}
				}
				switch proto {
				case "vmess":
					outbound["uuid"] = user.UUID
					outbound["security"] = "auto"
				case "vless":
					outbound["uuid"] = user.UUID
				case "shadowsocks":
					outbound["method"] = n.Cipher
					outbound["password"] = user.UUID
				case "trojan", "anytls", "hysteria2":
					outbound["password"] = user.UUID
				case "tuic":
					outbound["uuid"] = user.UUID
					outbound["password"] = user.UUID
					outbound["congestion_control"] = "cubic"
				default:
					outbound["username"] = user.UUID
					outbound["password"] = user.UUID
				}
				data, _ := json.Marshal(map[string]any{"log": map[string]any{"level": "error"}, "outbounds": []any{outbound}})
				ctx := include.Context(context.Background())
				opts, err := J.UnmarshalExtendedContext[option.Options](ctx, data)
				if err != nil {
					t.Fatal(err)
				}
				client, err := box.New(box.Options{Context: ctx, Options: opts})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { client.Close() })
				if err := client.Start(); err != nil {
					t.Fatal(err)
				}
				o, _ := client.Outbound().Outbound("proxy")
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				c, err := o.DialContext(ctx, "tcp", singM.ParseSocksaddr(echo.Addr().String()))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				return c
			}
			roundTrip := func(c net.Conn, wantSuccess bool) {
				t.Helper()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				payload := []byte("sing-box-1.14.4-upgrade")
				_, err := c.Write(payload)
				response := make([]byte, len(payload))
				if err == nil {
					_, err = io.ReadFull(c, response)
				}
				if wantSuccess && (err != nil || string(response) != string(payload)) {
					t.Fatalf("echo: %q, %v", response, err)
				}
				if !wantSuccess && err == nil {
					t.Fatal("removed user still authenticates")
				}
			}
			existing := dial(testUsers[0])
			roundTrip(existing, true)
			if _, err := s.AddUsers(testUsers[1:]); err != nil {
				t.Fatal(err)
			}
			roundTrip(existing, true)
			second := dial(testUsers[1])
			roundTrip(second, true)
			if s.box != original {
				t.Fatal("user update restarted box")
			}
			if _, err := s.RemoveUsers(testUsers[:1]); err != nil {
				t.Fatal(err)
			}
			roundTrip(second, true)
			// VMess/VLESS/SS defer the remote handshake until the first write.
			if proto == "vmess" || proto == "vless" || proto == "shadowsocks" {
				roundTrip(dial(testUsers[0]), false)
			}
			traffic, _, _, err := s.GetUserTraffic(context.Background())
			if err != nil || traffic[testUsers[1].ID][0] == 0 || traffic[testUsers[1].ID][1] == 0 {
				t.Fatalf("missing user traffic: %v, %v", traffic, err)
			}
			before := traffic[testUsers[1].ID]
			second.Close()
			existing.Close()
			// Invalid generated options must leave the old listener usable.
			bad := *n
			bad.ServerPort = -1
			if err := s.Reload(&bad, testUsers[1:], cert); err == nil {
				t.Fatal("accepted invalid port")
			}
			if s.box != original {
				t.Fatal("invalid config replaced running box")
			}
			roundTrip(dial(testUsers[1]), true)
			// A valid same-port reload must release and rebind its listener.
			if err := s.Reload(n, testUsers[1:], cert); err != nil {
				t.Fatal(err)
			}
			roundTrip(dial(testUsers[1]), true)
			after, _, _, _ := s.GetUserTraffic(context.Background())
			if after[testUsers[1].ID][0] < before[0] {
				t.Fatal("reload reset cumulative traffic")
			}
		})
	}
}

func TestUpgradePortConflictRollback(t *testing.T) {
	s := New(config.KernelConfig{LogLevel: "error"})
	defer s.Stop()
	n := &model.NodeSpec{Protocol: "socks", ServerPort: freeTestPort(t)}
	if err := s.Start(n, testUsers[:1], kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	bad := *n
	bad.ServerPort = busy.Addr().(*net.TCPAddr).Port
	if err := s.Reload(&bad, testUsers[:1], kernel.TLSCert{}); err == nil {
		t.Fatal("expected port conflict")
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", n.ServerPort), time.Second)
	if err != nil {
		t.Fatalf("old listener was not restored: %v", err)
	}
	c.Close()
}
