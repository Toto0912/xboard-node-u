package cert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/caddyserver/certmagic"

	"github.com/cedar2025/xboard-node/internal/config"
)

// triggerOnChildContext runs at the point startACME derives the candidate
// runtime's context, after it detached the previous runtime and before obtain.
type triggerOnChildContext struct {
	context.Context
	once      sync.Once
	onPrepare func()
}

func (c *triggerOnChildContext) Done() <-chan struct{} {
	c.once.Do(c.onPrepare)
	return c.Context.Done()
}

func TestACMERollbackRetainsRenewalThroughServiceRestore(t *testing.T) {
	for _, scenario := range []string{"prepare fails", "same ACME rollback", "port-only rollback"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			const domain = "a.example.com"
			cfg := config.CertConfig{CertMode: "http", Domain: domain, CertDir: dir}
			a1, k1 := generateLongLivedPair(t, domain)
			a2, k2 := generateLongLivedPair(t, domain)
			seedCertMagicStorage(t, dir, domain, a1, k1)
			m := NewManager(cfg)
			t.Cleanup(m.Stop)
			if err := m.Start(ctx); err != nil {
				t.Fatal(err)
			}
			stale := m.TLSCert()
			rt := m.activeACME
			issuer := rt.magic.Issuers[0].IssuerKey()
			storage := &certmagic.FileStorage{Path: dir}
			seedCertMagicStorage(t, dir, domain, a2, k2)
			renew := func() { m.handleACMERenewal(ctx, rt, storage, issuer, domain) }

			switch scenario {
			case "prepare fails":
				const otherDomain = "b.example.com"
				b, _ := generateLongLivedPair(t, otherDomain)
				// The triple exists, so no network issuance takes place. The
				// deliberately mismatched private key fails candidate preparation.
				seedCertMagicStorage(t, dir, otherDomain, b, k1)
				during := &triggerOnChildContext{Context: ctx, onPrepare: renew}
				if _, err := m.Reconfigure(during, config.CertConfig{CertMode: "http", Domain: otherDomain}); err == nil {
					t.Fatal("candidate with mismatched key succeeded")
				}
			case "same ACME rollback":
				if _, err := m.Reconfigure(ctx, cfg); err != nil {
					t.Fatal(err)
				}
				renew()
			case "port-only rollback":
				renew()
			}
			m.Restore(cfg, stale)
			m.Restore(cfg, stale)
			cert, seq, pending := m.PendingRenewalSnapshot()
			if !pending || !bytes.Equal(cert.CertPEM, a2) || m.activeACME != rt {
				t.Fatal("rollback did not retain original runtime and latest renewal")
			}
			disk, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
			if err != nil || !bytes.Equal(disk, a2) {
				t.Fatalf("persisted renewal mismatch: %v", err)
			}
			pair, err := tls.X509KeyPair(cert.CertPEM, cert.KeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
			server.StartTLS()
			defer server.Close()
			client := server.Client()
			client.Transport.(*http.Transport).TLSClientConfig.ServerName = domain
			res, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			want, _ := pem.Decode(a2)
			if !bytes.Equal(res.TLS.PeerCertificates[0].Raw, want.Bytes) {
				t.Fatal("TLS handshake did not present the renewed leaf")
			}
			m.AckRenewedThrough(seq)
			if m.HasPendingRenewal() {
				t.Fatal("duplicate Restore generated a spurious renewal")
			}
		})
	}
}

func TestAcknowledgingLoadedPEMRetainsLaterRenewal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const domain = "sequence.example.com"
	cfg := config.CertConfig{CertMode: "http", Domain: domain, CertDir: dir}
	first, keyFirst := generateLongLivedPair(t, domain)
	seedCertMagicStorage(t, dir, domain, first, keyFirst)
	m := NewManager(cfg)
	t.Cleanup(m.Stop)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rt := m.activeACME
	store := &certmagic.FileStorage{Path: dir}
	issuer := rt.magic.Issuers[0].IssuerKey()
	renew := func() []byte {
		cert, key := generateLongLivedPair(t, domain)
		seedCertMagicStorage(t, dir, domain, cert, key)
		if !m.handleACMERenewal(ctx, rt, store, issuer, domain) {
			t.Fatal("active renewal ignored")
		}
		return cert
	}
	second := renew()
	loaded, seq, ok := m.PendingRenewalSnapshot()
	third := renew()
	m.AckRenewedThrough(seq)
	next, nextSeq, pending := m.PendingRenewalSnapshot()
	if !ok || !bytes.Equal(loaded.CertPEM, second) || !pending || nextSeq <= seq || !bytes.Equal(next.CertPEM, third) {
		t.Fatal("acknowledging the loaded certificate lost a newer renewal")
	}
	m.AckRenewedThrough(nextSeq)
	if m.HasPendingRenewal() {
		t.Fatal("acknowledged generation remains pending")
	}
}

func TestCandidateRenewalFollowsCommitOrRollback(t *testing.T) {
	for _, action := range []string{"commit", "rollback"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			ctx := context.Background()
			const domain = "candidate.example.com"
			base := config.CertConfig{CertMode: "none", CertDir: dir}
			m := NewManager(base)
			t.Cleanup(m.Stop)
			empty := m.TLSCert()
			b1, k1 := generateLongLivedPair(t, domain)
			b2, k2 := generateLongLivedPair(t, domain)
			seedCertMagicStorage(t, dir, domain, b1, k1)
			if _, err := m.Reconfigure(ctx, config.CertConfig{CertMode: "http", Domain: domain}); err != nil {
				t.Fatal(err)
			}
			rt := m.activeACME
			seedCertMagicStorage(t, dir, domain, b2, k2)
			m.handleACMERenewal(ctx, rt, &certmagic.FileStorage{Path: dir}, rt.magic.Issuers[0].IssuerKey(), domain)
			if m.HasPendingRenewal() {
				t.Fatal("uncommitted candidate published a reload")
			}
			if action == "rollback" {
				m.Restore(base, empty)
				if m.HasCert() || m.HasPendingRenewal() || m.activeACME != nil || rt.cache != nil {
					t.Fatal("rollback retained candidate state")
				}
				if _, err := os.Stat(filepath.Join(dir, "cert.pem")); !os.IsNotExist(err) {
					t.Fatalf("rolled-back candidate was published: %v", err)
				}
				return
			}
			m.Commit()
			cert, _, pending := m.PendingRenewalSnapshot()
			disk, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
			if !pending || !bytes.Equal(cert.CertPEM, b2) || err != nil || !bytes.Equal(disk, b2) {
				t.Fatalf("commit did not publish latest candidate renewal: %v", err)
			}
		})
	}
}
