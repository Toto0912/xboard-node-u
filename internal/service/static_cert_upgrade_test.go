package service

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
)

func TestPanelCertOverridePreservesStaticUpgradeFingerprint(t *testing.T) {
	for _, parentName := range []string{"certs", "custom-parent-certs"} {
		for _, mode := range []string{"self", "http"} {
			t.Run(parentName+"/"+mode, func(t *testing.T) {
				base := t.TempDir()
				legacy := filepath.Join(base, parentName)
				if err := os.MkdirAll(legacy, 0o700); err != nil {
					t.Fatal(err)
				}
				domain := "upgrade.example.com"
				// DNS-provisioned wildcard certificates can already be present in the
				// old store. The HTTP-mode manager only needs to reuse this fixture.
				if mode == "http" && parentName == "custom-parent-certs" {
					domain = "*.upgrade.example.com"
				}
				certPEM, keyPEM := generateLongLivedTestPEM(t, domain)
				if mode == "http" {
					seedServiceACMEStorage(t, legacy, domain, certPEM, keyPEM)
				} else {
					if err := os.WriteFile(filepath.Join(legacy, "cert.pem"), certPEM, 0o644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(legacy, "key.pem"), keyPEM, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				parent := config.Config{
					Kernel: config.KernelConfig{Type: "singbox", ConfigDir: base},
					Cert:   config.CertConfig{CertDir: legacy, CertMode: "none"},
					Nodes:  []config.NodeEntry{{NodeID: 7}},
				}
				node := parent.ExpandNodes()[0]
				localDir := node.Cert.CertDir
				if localDir == legacy || node.Cert.LegacyCertDir != legacy {
					t.Fatalf("unexpected expanded certificate directories: %+v", node.Cert)
				}
				s := New(node)
				defer s.cert.Stop()
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				if err := s.cert.Start(ctx); err != nil {
					t.Fatal(err)
				}
				changed, err := s.applyRemoteOverrides(ctx, &model.NodeSpec{CertConfig: &config.CertConfig{
					CertMode: mode, Domain: domain, CertDir: filepath.Join(base, "panel-dir"),
				}})
				if !changed || err != nil {
					t.Fatalf("apply panel certificate: changed=%v err=%v", changed, err)
				}
				if s.cfg.Cert.CertDir != localDir || s.cfg.Cert.LegacyCertDir != legacy {
					t.Fatalf("panel override lost local directories: %+v", s.cfg.Cert)
				}
				k := &tlsListenerKernel{}
				if err := k.listenWithTLS(s.cert.TLSCert()); err != nil {
					t.Fatal(err)
				}
				defer k.Stop()
				s.cert.Commit()

				roots := x509.NewCertPool()
				if !roots.AppendCertsFromPEM(certPEM) {
					t.Fatal("invalid fixture certificate")
				}
				serverName := strings.Replace(domain, "*.", "host.", 1)
				k.mu.Lock()
				addr := k.addr
				k.mu.Unlock()
				conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: roots, ServerName: serverName})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				expected, _ := pem.Decode(certPEM)
				presented := conn.ConnectionState().PeerCertificates[0]
				if sha256.Sum256(presented.Raw) != sha256.Sum256(expected.Bytes) {
					t.Fatal("remote override presented a new fingerprint after upgrade")
				}
			})
		}
	}
}
