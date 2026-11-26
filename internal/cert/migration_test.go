package cert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/cedar2025/xboard-node/internal/config"
)

func TestMigrateStaticSelfCertLayouts(t *testing.T) {
	const domain = "migration.example.com"
	cert, key := generateLongLivedPair(t, domain)
	certName, keyName := selfFileNames(domain)
	for _, names := range [][2]string{{certName, keyName}, {"self-cert.pem", "self-key.pem"}, {"cert.pem", "key.pem"}} {
		t.Run(names[0], func(t *testing.T) {
			base := t.TempDir()
			old := filepath.Join(base, "certs")
			if err := os.Mkdir(old, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(old, names[0]), cert, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(old, names[1]), key, 0o600); err != nil {
				t.Fatal(err)
			}
			parent := config.Config{Kernel: config.KernelConfig{ConfigDir: base}, Cert: config.CertConfig{CertDir: old, CertMode: "self", Domain: domain}, Nodes: []config.NodeEntry{{NodeID: 7}}}
			cfg := parent.ExpandNodes()[0].Cert
			m := NewManager(cfg)
			defer m.Stop()
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(m.TLSCert().CertPEM, cert) {
				t.Fatal("self-signed certificate fingerprint changed")
			}
			for _, path := range []string{filepath.Join(old, names[0]), filepath.Join(cfg.CertDir, certName)} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, cert) {
					t.Fatalf("migration material missing at %s: %v", path, err)
				}
			}
		})
	}
}

func TestStaticACMEUpgradeThroughReconfigure(t *testing.T) {
	const domain = "upgrade.example.com"
	cert, key := generateLongLivedPair(t, domain)
	for _, parentDir := range []string{"certs", "custom-parent"} {
		t.Run(parentDir, func(t *testing.T) {
			base := t.TempDir()
			legacy := filepath.Join(base, parentDir)
			seedCertMagicStorage(t, legacy, domain, cert, key)
			parent := config.Config{
				Kernel: config.KernelConfig{ConfigDir: base},
				Cert:   config.CertConfig{CertDir: legacy, CertMode: "none"},
				Nodes:  []config.NodeEntry{{NodeID: 5}},
			}
			initial := parent.ExpandNodes()[0].Cert
			m := NewManager(initial)
			defer m.Stop()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if _, err := m.Reconfigure(ctx, config.CertConfig{CertMode: "http", Domain: domain}); err != nil {
				t.Fatal(err)
			}
			m.Commit()
			if !bytes.Equal(m.TLSCert().CertPEM, cert) {
				t.Fatal("ACME certificate fingerprint changed on upgrade")
			}
			issuer := (&certmagic.ACMEIssuer{CA: certmagic.LetsEncryptProductionCA}).IssuerKey()
			for _, dir := range []string{initial.CertDir, legacy} {
				site, err := loadSite(ctx, &certmagic.FileStorage{Path: dir}, issuer, domain)
				if err != nil || !bytes.Equal(site.key, key) {
					t.Fatalf("bad site in %s: %v", dir, err)
				}
			}
		})
	}
}

func TestMigrateACMESiteRepairAndRetry(t *testing.T) {
	ctx := context.Background()
	const domain = "migration.example.com"
	old := &certmagic.FileStorage{Path: t.TempDir()}
	dst := &certmagic.FileStorage{Path: t.TempDir()}
	issuer := (&certmagic.ACMEIssuer{CA: certmagic.LetsEncryptProductionCA}).IssuerKey()
	cert, key := generateLongLivedPair(t, domain)
	seedCertMagicStorage(t, old.Path, domain, cert, key)
	otherCert, otherKey := generateLongLivedPair(t, domain)
	seedCertMagicStorage(t, dst.Path, domain, otherCert, otherKey)
	if err := migrateACMESite(ctx, dst, old.Path, issuer, domain); err != nil {
		t.Fatal(err)
	}
	site, err := loadSite(ctx, dst, issuer, domain)
	if err != nil || !bytes.Equal(site.cert, otherCert) {
		t.Fatalf("valid target was replaced: %v", err)
	}

	meta := certmagic.StorageKeys.SiteMeta(issuer, domain)
	if err := dst.Store(ctx, meta, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMESite(ctx, dst, old.Path, issuer, domain); err != nil {
		t.Fatal(err)
	}
	site, err = loadSite(ctx, dst, issuer, domain)
	if err != nil || !bytes.Equal(site.cert, cert) || !bytes.Equal(site.key, key) {
		t.Fatalf("broken target not recovered: %v", err)
	}

	certFile := dst.Filename(certmagic.StorageKeys.SiteCert(issuer, domain))
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(certFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMESite(ctx, dst, old.Path, issuer, domain); err == nil {
		t.Fatal("destination I/O failure ignored")
	}
	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if err := migrateACMESite(ctx, dst, old.Path, issuer, domain); err != nil {
		t.Fatal(err)
	}
	site, err = loadSite(ctx, dst, issuer, domain)
	if err != nil || !bytes.Equal(site.cert, cert) {
		t.Fatalf("retry lost original identity: %v", err)
	}
}

func TestNoneCleanupKeepsKeyWhenPairOwnershipUncertain(t *testing.T) {
	cert, key := generateLongLivedPair(t, "owner.example.com")
	_, otherKey := generateLongLivedPair(t, "other.example.com")
	for _, change := range []string{"key replaced", "cert missing"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			m := NewManager(config.CertConfig{CertDir: dir, CertMode: "content", CertContent: string(cert), KeyContent: string(key)})
			defer m.Stop()
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
			want := key
			if change == "key replaced" {
				want = otherKey
				if err := os.WriteFile(keyPath, want, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(certPath); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Reconfigure(context.Background(), config.CertConfig{CertDir: dir, CertMode: "none"}); err != nil {
				t.Fatal(err)
			}
			m.Commit()
			got, err := os.ReadFile(keyPath)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("unverified key removed or changed: %v", err)
			}
			if change == "key replaced" {
				if _, err := os.Stat(certPath); err != nil {
					t.Fatal("matching cert deleted despite key mismatch", err)
				}
			}
		})
	}
}

func TestSelfSignatureSANAndDates(t *testing.T) {
	pubKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tc := range []struct {
		name, domain string
		signer       *ecdsa.PrivateKey
		from, to     time.Time
		valid        bool
	}{
		{"ip", "192.0.2.1", pubKey, now.Add(-time.Hour), now.Add(time.Hour), true},
		{"san", "valid.example", pubKey, now.Add(-time.Hour), now.Add(time.Hour), true},
		{"wrong signing key", "valid.example", other, now.Add(-time.Hour), now.Add(time.Hour), false},
		{"cn only", "cn.example", pubKey, now.Add(-time.Hour), now.Add(time.Hour), false},
		{"expired", "valid.example", pubKey, now.Add(-2 * time.Hour), now.Add(-time.Hour), false},
		{"not yet valid", "valid.example", pubKey, now.Add(time.Hour), now.Add(2 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tpl := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cn.example"}, DNSNames: []string{"valid.example"}, IPAddresses: []net.IP{net.ParseIP("192.0.2.1")}, NotBefore: tc.from, NotAfter: tc.to, KeyUsage: x509.KeyUsageDigitalSignature}
			der, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &pubKey.PublicKey, tc.signer)
			if err != nil {
				t.Fatal(err)
			}
			if got := certMatchesSelfDomain(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), tc.domain); got != tc.valid {
				t.Errorf("match=%v, want %v", got, tc.valid)
			}
		})
	}
}
