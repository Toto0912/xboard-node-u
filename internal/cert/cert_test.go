package cert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
)

// generateSelfSignedPair returns a fresh self-signed certificate and matching
// private key in PEM form, suitable for storing as a libdns/certmagic site
// resource in tests.
func generateSelfSignedPair(t *testing.T, domain string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

// TestLoadPEMFromStorage_RestartScenario simulates the warm-restart bug fix:
// certmagic.ObtainCertSync returns nil without firing cert_obtained when a
// valid cert already exists in storage. The manager must still populate its
// in-memory PEM cache by reading storage explicitly.
func TestLoadPEMFromStorage_RestartScenario(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"

	certPEM, keyPEM := generateSelfSignedPair(t, domain)

	ctx := context.Background()
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certPEM); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyPEM); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	if m.HasCert() {
		t.Fatal("manager should start with no cert in memory")
	}

	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("loadPEMFromStorage: %v", err)
	}
	if !m.HasCert() {
		t.Fatal("expected HasCert() == true after explicit load")
	}
	got := m.TLSCert()
	if string(got.CertPEM) != string(certPEM) {
		t.Errorf("certPEM mismatch")
	}
	if string(got.KeyPEM) != string(keyPEM) {
		t.Errorf("keyPEM mismatch")
	}
}

// TestLoadPEMFromStorage_RenewalRefresh confirms that calling load a second
// time with newer bytes on disk swaps the in-memory cache atomically — this
// is the renewal hot-reload path triggered from the cert_obtained event.
func TestLoadPEMFromStorage_RenewalRefresh(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"
	ctx := context.Background()

	certV1, keyV1 := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certV1); err != nil {
		t.Fatalf("seed v1 cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyV1); err != nil {
		t.Fatalf("seed v1 key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	certV2, keyV2 := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certV2); err != nil {
		t.Fatalf("seed v2 cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyV2); err != nil {
		t.Fatalf("seed v2 key: %v", err)
	}

	if err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain); err != nil {
		t.Fatalf("refresh load: %v", err)
	}
	got := m.TLSCert()
	if string(got.CertPEM) != string(certV2) {
		t.Error("expected v2 cert after refresh")
	}
	if string(got.KeyPEM) != string(keyV2) {
		t.Error("expected v2 key after refresh")
	}
}

// TestLoadPEMFromStorage_MissingFiles ensures we surface a clear error when
// storage doesn't have the expected keys (defensive — should not happen in
// practice because ObtainCertSync guarantees presence on success).
func TestLoadPEMFromStorage_MissingFiles(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	m := NewManager(config.CertConfig{})
	err := m.loadPEMFromStorage(context.Background(), storage, "test-issuer", "example.test")
	if err == nil {
		t.Fatal("expected error when storage is empty")
	}
}

// TestLoadPEMFromStorage_RejectsMismatchedPair guards against silently loading
// a cert and key that do not pair (e.g. partial write or external tampering).
func TestLoadPEMFromStorage_RejectsMismatchedPair(t *testing.T) {
	dir := t.TempDir()
	storage := &certmagic.FileStorage{Path: dir}
	const domain = "example.test"
	const issuerKey = "test-issuer"
	ctx := context.Background()

	certA, _ := generateSelfSignedPair(t, domain)
	_, keyB := generateSelfSignedPair(t, domain)
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certA); err != nil {
		t.Fatalf("seed cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyB); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	m := NewManager(config.CertConfig{})
	err := m.loadPEMFromStorage(ctx, storage, issuerKey, domain)
	if err == nil {
		t.Fatal("expected error for mismatched cert/key pair")
	}
	if m.HasCert() {
		t.Error("manager should not store invalid pair")
	}
	_ = filepath.Join(dir, "stub")
}

// TestACMEFingerprint covers the config-equality logic that decides whether
// Reconfigure must tear down a running certmagic instance.
func TestACMEFingerprint(t *testing.T) {
	base := config.CertConfig{
		CertMode:    "dns",
		Domain:      "a.example.com",
		Email:       "ops@example.com",
		DNSProvider: "cloudflare",
		DNSEnv:      map[string]string{"CLOUDFLARE_API_TOKEN": "tok"},
	}

	cases := []struct {
		name    string
		mutate  func(*config.CertConfig)
		differs bool
	}{
		{"same", func(*config.CertConfig) {}, false},
		{"domain", func(c *config.CertConfig) { c.Domain = "b.example.com" }, true},
		{"mode", func(c *config.CertConfig) { c.CertMode = "http" }, true},
		{"email", func(c *config.CertConfig) { c.Email = "other@example.com" }, true},
		{"provider", func(c *config.CertConfig) { c.DNSProvider = "alidns" }, true},
		{"env_value", func(c *config.CertConfig) { c.DNSEnv = map[string]string{"CLOUDFLARE_API_TOKEN": "other"} }, true},
		{"env_key_only", func(c *config.CertConfig) { c.DNSEnv = map[string]string{"OTHER": "tok"} }, true},
	}

	baseFp := acmeFingerprint(base)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			if tc.mutate != nil {
				// deep-copy DNSEnv before mutation to avoid sharing
				cfg.DNSEnv = map[string]string{}
				for k, v := range base.DNSEnv {
					cfg.DNSEnv[k] = v
				}
				tc.mutate(&cfg)
			}
			got := acmeFingerprint(cfg)
			if (got != baseFp) != tc.differs {
				t.Errorf("differs=%v but got==base=%v", tc.differs, got == baseFp)
			}
		})
	}
}

// TestACMEFingerprintEnvOrderStable ensures map iteration order does not
// affect the fingerprint (must be sorted internally).
func TestACMEFingerprintEnvOrderStable(t *testing.T) {
	a := config.CertConfig{
		CertMode: "dns", Domain: "x", DNSProvider: "p",
		DNSEnv: map[string]string{"A": "1", "B": "2", "C": "3"},
	}
	b := config.CertConfig{
		CertMode: "dns", Domain: "x", DNSProvider: "p",
		DNSEnv: map[string]string{"C": "3", "A": "1", "B": "2"},
	}
	if acmeFingerprint(a) != acmeFingerprint(b) {
		t.Fatal("fingerprint not order-independent")
	}
}

func TestManager_NotifyRenewedSignalsChannelUntilAcked(t *testing.T) {
	m := NewManager(config.CertConfig{})
	if m.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() == false initially")
	}

	m.notifyRenewed()
	if !m.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() == true after notifyRenewed")
	}

	select {
	case <-m.RenewedCh():
	default:
		t.Fatal("expected RenewedCh() to receive signal")
	}

	// Reading the signal alone must not clear pending status until explicit AckRenewed.
	if !m.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() to remain true before AckRenewed")
	}
	m.AckRenewed()
	if m.HasPendingRenewal() {
		t.Fatal("expected HasPendingRenewal() == false after AckRenewed")
	}
}

func generateLongLivedPair(t *testing.T, domain string) (certPEM, keyPEM []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: domain},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return
}

func seedCertMagicStorage(t *testing.T, certDir, domain string, certPEM, keyPEM []byte) {
	t.Helper()
	ctx := context.Background()
	storage := &certmagic.FileStorage{Path: certDir}
	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	defer cache.Stop()
	magic = certmagic.New(cache, certmagic.Config{Storage: storage})
	issuerKey := certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
		CA: certmagic.LetsEncryptProductionCA,
	}).IssuerKey()

	if err := storage.Store(ctx, certmagic.StorageKeys.SiteCert(issuerKey, domain), certPEM); err != nil {
		t.Fatalf("store site cert: %v", err)
	}
	if err := storage.Store(ctx, certmagic.StorageKeys.SitePrivateKey(issuerKey, domain), keyPEM); err != nil {
		t.Fatalf("store site key: %v", err)
	}
	meta := []byte(fmt.Sprintf(`{"sans":[%q]}`, domain))
	if err := storage.Store(ctx, certmagic.StorageKeys.SiteMeta(issuerKey, domain), meta); err != nil {
		t.Fatalf("store site meta: %v", err)
	}
}

func TestManager_DeferredPersistDoesNotOverwriteDiskOnRollback(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	selfCfg := config.CertConfig{CertMode: "self", Domain: "self.example.com", CertDir: dir}
	m := NewManager(selfCfg)
	defer m.Stop()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start self cert: %v", err)
	}
	selfTLS := m.TLSCert()

	// Candidate file cert B.
	certB, keyB := generateLongLivedPair(t, "fileb.example.com")
	fileBDir := t.TempDir()
	certBPath := filepath.Join(fileBDir, "b.crt")
	keyBPath := filepath.Join(fileBDir, "b.key")
	if err := os.WriteFile(certBPath, certB, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyBPath, keyB, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := m.Reconfigure(ctx, config.CertConfig{
		CertMode: "file",
		CertFile: certBPath,
		KeyFile:  keyBPath,
		CertDir:  dir,
	})
	if err != nil || !changed {
		t.Fatalf("Reconfigure(file B) changed=%v err=%v", changed, err)
	}

	// Before Commit, disk must preserve domain-scoped self cert without writing cert.pem for B.
	selfCertName, _ := selfFileNames("self.example.com")
	onDiskSelf, err := os.ReadFile(filepath.Join(dir, selfCertName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDiskSelf, selfTLS.CertPEM) {
		t.Fatal("expected Reconfigure before Commit NOT to overwrite persisted self cert")
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); !os.IsNotExist(err) {
		t.Fatalf("expected uncommitted file B not to create cert.pem yet, stat err = %v", err)
	}

	// Simulate kernel rollout failure -> Restore(self).
	m.Restore(selfCfg, selfTLS)
	restarted := NewManager(selfCfg)
	if err := restarted.Start(ctx); err != nil {
		t.Fatalf("restarted self Start: %v", err)
	}
	if !bytes.Equal(restarted.TLSCert().CertPEM, selfTLS.CertPEM) {
		t.Fatal("expected restarted self-signed manager to reload original cert A, not rolled-back cert B")
	}

	// Re-apply file B and Commit() -> disk copy updates to B.
	if _, err := m.Reconfigure(ctx, config.CertConfig{
		CertMode: "file",
		CertFile: certBPath,
		KeyFile:  keyBPath,
		CertDir:  dir,
	}); err != nil {
		t.Fatalf("Reconfigure(file B #2): %v", err)
	}
	m.Commit()
	onDiskAfterCommit, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDiskAfterCommit, certB) {
		t.Fatal("expected Commit() to persist cert B to disk")
	}
}

func TestManager_ACMERollbackAndRetryReloadsACMECertAndStopClears(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// File cert A.
	certA, keyA := generateLongLivedPair(t, "filea.example.com")
	fileADir := t.TempDir()
	certAPath := filepath.Join(fileADir, "a.crt")
	keyAPath := filepath.Join(fileADir, "a.key")
	if err := os.WriteFile(certAPath, certA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyAPath, keyA, 0o600); err != nil {
		t.Fatal(err)
	}
	fileACfg := config.CertConfig{CertMode: "file", CertFile: certAPath, KeyFile: keyAPath, CertDir: dir}
	m := NewManager(fileACfg)
	defer m.Stop()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("start file A: %v", err)
	}

	// Pre-seed ACME cert B into CertMagic FileStorage.
	certB, keyB := generateLongLivedPair(t, "acmeb.example.com")
	seedCertMagicStorage(t, dir, "acmeb.example.com", certB, keyB)

	acmeBCfg := config.CertConfig{CertMode: "http", Domain: "acmeb.example.com", CertDir: dir}
	changed, err := m.Reconfigure(ctx, acmeBCfg)
	if err != nil || !changed {
		t.Fatalf("Reconfigure(ACME B #1) changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(m.TLSCert().CertPEM, certB) {
		t.Fatal("expected Reconfigure(ACME B) to load cert B")
	}

	// Simulate kernel rollout failure -> Restore back to file A.
	m.Restore(fileACfg, kernel.TLSCert{CertPEM: certA, KeyPEM: keyA})
	if m.acmeStarted {
		t.Fatal("expected Restore back to file mode to clear acmeStarted")
	}
	if m.acmeFingerprint != "" {
		t.Fatal("expected Restore back to file mode to clear acmeFingerprint")
	}
	if !bytes.Equal(m.TLSCert().CertPEM, certA) {
		t.Fatal("expected Restore to restore cert A PEM")
	}

	// Retry switching to ACME B: must NOT hit stale acmeStarted early return!
	changed, err = m.Reconfigure(ctx, acmeBCfg)
	if err != nil || !changed {
		t.Fatalf("Reconfigure(ACME B #2 retry) changed=%v err=%v", changed, err)
	}
	if !bytes.Equal(m.TLSCert().CertPEM, certB) {
		t.Fatal("expected ACME B retry after Restore to load cert B, not stay on cert A")
	}
	m.Commit()
}

func TestManager_ReconfigureToNoneClearsMemoryAndPersistedCert(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	certA, keyA := generateLongLivedPair(t, "content.example.com")

	m := NewManager(config.CertConfig{CertMode: "none", CertDir: dir})
	defer m.Stop()
	if _, err := m.Reconfigure(ctx, config.CertConfig{
		CertMode:    "content",
		CertContent: string(certA),
		KeyContent:  string(keyA),
		CertDir:     dir,
	}); err != nil {
		t.Fatalf("Reconfigure(content): %v", err)
	}
	m.Commit()
	if !m.HasCert() {
		t.Fatal("expected HasCert() == true")
	}

	changed, err := m.Reconfigure(ctx, config.CertConfig{CertMode: "none", CertDir: dir})
	if err != nil || !changed {
		t.Fatalf("Reconfigure(none) changed=%v err=%v", changed, err)
	}
	m.Commit()
	if m.HasCert() {
		t.Fatal("expected HasCert() == false after switching to none")
	}
	if _, err := os.Stat(filepath.Join(dir, "cert.pem")); !os.IsNotExist(err) {
		t.Fatalf("expected persisted cert.pem to be removed on none commit, stat err = %v", err)
	}
}

func TestManager_SelfCertIsolatedFromPanelOverrideDiskCopy(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	selfCfg := config.CertConfig{CertMode: "self", Domain: "selfa.example.com", CertDir: dir}
	m := NewManager(selfCfg)
	defer m.Stop()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	selfCertA := append([]byte(nil), m.TLSCert().CertPEM...)

	// Commit panel override B.
	certB, keyB := generateLongLivedPair(t, "panelb.example.com")
	if _, err := m.Reconfigure(ctx, config.CertConfig{
		CertMode:    "content",
		CertContent: string(certB),
		KeyContent:  string(keyB),
		CertDir:     dir,
	}); err != nil {
		t.Fatal(err)
	}
	m.Commit()
	if !bytes.Equal(m.TLSCert().CertPEM, certB) {
		t.Fatal("expected active cert to be panel B")
	}

	// Switch back to local self mode: must load original self-cert.pem A, NOT cert.pem B!
	if _, err := m.Reconfigure(ctx, selfCfg); err != nil {
		t.Fatal(err)
	}
	m.Commit()
	if !bytes.Equal(m.TLSCert().CertPEM, selfCertA) {
		t.Fatal("expected restoring self mode after committed panel override to load original self cert A, not panel cert B")
	}
}

func TestManager_NoneCommitDoesNotDeleteUnownedPersistedCert(t *testing.T) {
	sharedDir := t.TempDir()
	ctx := context.Background()
	certA, keyA := generateLongLivedPair(t, "nodea.example.com")

	// Node A commits its certificate to sharedDir.
	mA := NewManager(config.CertConfig{
		CertMode:    "content",
		CertContent: string(certA),
		KeyContent:  string(keyA),
		CertDir:     sharedDir,
	})
	defer mA.Stop()
	if err := mA.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// Node B starts in none mode with the same directory and commits initial none config.
	mB := NewManager(config.CertConfig{CertMode: "none", CertDir: sharedDir})
	defer mB.Stop()
	if err := mB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	mB.Commit()

	onDisk, err := os.ReadFile(filepath.Join(sharedDir, "cert.pem"))
	if err != nil {
		t.Fatalf("expected unowned cert.pem to remain intact after none Commit(), err=%v", err)
	}
	if !bytes.Equal(onDisk, certA) {
		t.Fatal("expected cert.pem content to remain node A's cert")
	}
}

func TestManager_StaleACMERenewalCallbackDoesNotOverwriteCandidateOrDisk(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	storage := &certmagic.FileStorage{Path: dir}

	certA1, keyA1 := generateLongLivedPair(t, "acmea.example.com")
	seedCertMagicStorage(t, dir, "acmea.example.com", certA1, keyA1)
	certB, keyB := generateLongLivedPair(t, "acmeb.example.com")
	seedCertMagicStorage(t, dir, "acmeb.example.com", certB, keyB)

	m := NewManager(config.CertConfig{CertMode: "http", Domain: "acmea.example.com", CertDir: dir})
	defer m.Stop()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start ACME A: %v", err)
	}
	rtA := m.activeACME
	issuerKey := rtA.magic.Issuers[0].IssuerKey()

	// Seed a renewed cert A2 in storage before old ACME A fires its late callback.
	certA2, keyA2 := generateLongLivedPair(t, "acmea-renewed.example.com")
	seedCertMagicStorage(t, dir, "acmea.example.com", certA2, keyA2)

	// Switch to ACME B (uncommitted while kernel starts).
	if _, err := m.Reconfigure(ctx, config.CertConfig{CertMode: "http", Domain: "acmeb.example.com", CertDir: dir}); err != nil {
		t.Fatalf("Reconfigure ACME B: %v", err)
	}

	// Old ACME A's renewal callback finishes AFTER B was already loaded (both direct and concurrent under -race).
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if applied := m.handleACMERenewal(ctx, rtA, storage, issuerKey, "acmea.example.com"); applied {
				t.Error("expected stale ACME A renewal callback to be ignored")
			}
		}()
	}
	wg.Wait()

	if m.HasPendingRenewal() {
		t.Fatal("stale ACME A callback must NOT set pending renewal flag")
	}

	// Verify real TLS handshake using m.TLSCert() presents b.example.com, NOT acmea!
	tlsMat := m.TLSCert()
	pair, err := tls.X509KeyPair(tlsMat.CertPEM, tlsMat.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr == nil {
			defer c.Close()
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName
	_ = conn.Close()
	if cn != "acmeb.example.com" {
		t.Fatalf("TLS handshake CN = %q, want acmeb.example.com", cn)
	}

	// Commit B and verify persisted disk cert.pem is B, not A2.
	m.Commit()
	onDisk, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, certB) {
		t.Fatal("expected disk cert.pem after Commit() to be cert B, not overwritten by stale ACME A renewal")
	}
}

func TestManager_SelfCertLegacyMigrationAndDomainIsolation(t *testing.T) {
	ctx := context.Background()

	// Part A: Legacy v2.3.0 cert.pem/key.pem self-signed cert for legacy.example.com is reused and migrated.
	dirA := t.TempDir()
	legacyCert, legacyKey := generateLongLivedPair(t, "legacy.example.com")
	if err := os.WriteFile(filepath.Join(dirA, "cert.pem"), legacyCert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirA, "key.pem"), legacyKey, 0o600); err != nil {
		t.Fatal(err)
	}

	mA := NewManager(config.CertConfig{CertMode: "self", Domain: "legacy.example.com", CertDir: dirA})
	defer mA.Stop()
	if err := mA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mA.TLSCert().CertPEM, legacyCert) {
		t.Fatal("expected v2.3.0 legacy self cert.pem to be reused so pinned clients remain connected")
	}
	migratedName, _ := selfFileNames("legacy.example.com")
	migratedBytes, err := os.ReadFile(filepath.Join(dirA, migratedName))
	if err != nil {
		t.Fatalf("expected migrated domain-scoped self cert %s, err=%v", migratedName, err)
	}
	if !bytes.Equal(migratedBytes, legacyCert) {
		t.Fatal("expected migrated self cert content to match legacy cert")
	}

	// Part B: Switching from self domain A to self domain B must generate distinct cert for B, not reuse A!
	dirB := t.TempDir()
	mB := NewManager(config.CertConfig{CertMode: "self", Domain: "a.example.com", CertDir: dirB})
	defer mB.Stop()
	if err := mB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	certDomA := append([]byte(nil), mB.TLSCert().CertPEM...)

	if _, err := mB.Reconfigure(ctx, config.CertConfig{CertMode: "self", Domain: "b.example.com", CertDir: dirB}); err != nil {
		t.Fatal(err)
	}
	mB.Commit()
	certDomB := append([]byte(nil), mB.TLSCert().CertPEM...)
	if bytes.Equal(certDomA, certDomB) {
		t.Fatal("expected domain B self cert to differ from domain A self cert")
	}
	if !certMatchesSelfDomain(certDomB, "b.example.com") {
		t.Fatal("expected generated domain B cert to match b.example.com")
	}
	if _, err := mB.Reconfigure(ctx, config.CertConfig{CertMode: "self", Domain: "a.example.com", CertDir: dirB}); err != nil {
		t.Fatal(err)
	}
	mB.Commit()
	if !bytes.Equal(mB.TLSCert().CertPEM, certDomA) {
		t.Fatal("expected switching back to domain A to reload original domain A cert")
	}
}

func TestManager_NoneCommitDoesNotDeleteOverwrittenOtherNodesCert(t *testing.T) {
	sharedDir := t.TempDir()
	ctx := context.Background()
	certA, keyA := generateLongLivedPair(t, "a.example.com")
	certB, keyB := generateLongLivedPair(t, "b.example.com")

	// Node A commits cert A to sharedDir.
	mA := NewManager(config.CertConfig{CertMode: "content", CertContent: string(certA), KeyContent: string(keyA), CertDir: sharedDir})
	defer mA.Stop()
	if err := mA.Start(ctx); err != nil {
		t.Fatal(err)
	}
	mA.Commit()

	// Node B overwrites sharedDir/cert.pem with cert B.
	mB := NewManager(config.CertConfig{CertMode: "content", CertContent: string(certB), KeyContent: string(keyB), CertDir: sharedDir})
	defer mB.Stop()
	if err := mB.Start(ctx); err != nil {
		t.Fatal(err)
	}
	mB.Commit()

	// Node A switches to none and commits: must NOT delete B's cert.pem because digest mismatches A's owned copy!
	if _, err := mA.Reconfigure(ctx, config.CertConfig{CertMode: "none", CertDir: sharedDir}); err != nil {
		t.Fatal(err)
	}
	mA.Commit()

	onDisk, err := os.ReadFile(filepath.Join(sharedDir, "cert.pem"))
	if err != nil {
		t.Fatalf("expected cert.pem overwritten by B to remain intact, err=%v", err)
	}
	if !bytes.Equal(onDisk, certB) {
		t.Fatal("expected disk cert.pem to still equal cert B")
	}
}

func TestManager_PendingOldACMERenewalAppliedWhenCandidateRollsBack(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	storage := &certmagic.FileStorage{Path: dir}

	certA1, keyA1 := generateLongLivedPair(t, "acmea1.example.com")
	seedCertMagicStorage(t, dir, "acmea.example.com", certA1, keyA1)
	certB, keyB := generateLongLivedPair(t, "acmeb.example.com")
	seedCertMagicStorage(t, dir, "acmeb.example.com", certB, keyB)

	cfgA := config.CertConfig{CertMode: "http", Domain: "acmea.example.com", CertDir: dir}
	m := NewManager(cfgA)
	defer m.Stop()
	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start ACME A1: %v", err)
	}
	rtA := m.activeACME
	issuerKey := rtA.magic.Issuers[0].IssuerKey()

	// Begin switching to ACME B (uncommitted while candidate kernel attempts start).
	if _, err := m.Reconfigure(ctx, config.CertConfig{CertMode: "http", Domain: "acmeb.example.com", CertDir: dir}); err != nil {
		t.Fatalf("Reconfigure ACME B: %v", err)
	}

	// While B is uncommitted, old ACME A finishes renewing A2 and stores it on disk.
	certA2, keyA2 := generateLongLivedPair(t, "acmea2-renewed.example.com")
	seedCertMagicStorage(t, dir, "acmea.example.com", certA2, keyA2)
	if applied := m.handleACMERenewal(ctx, rtA, storage, issuerKey, "acmea.example.com"); applied {
		t.Fatal("expected renewal for pendingOldACME not to overwrite candidate B yet")
	}
	if !bytes.Equal(m.TLSCert().CertPEM, certB) {
		t.Fatal("expected candidate cert B to stay active until rollback")
	}

	// Candidate B fails -> Restore rolls back to cfgA: MUST promote stashed A2, persist A2, and notify renewal!
	m.Restore(cfgA, kernel.TLSCert{CertPEM: certA1, KeyPEM: keyA1})
	if !m.HasPendingRenewal() {
		t.Fatal("expected Restore to raise pending renewal flag when pendingOldACME had completed renewal A2")
	}
	tlsMat := m.TLSCert()
	if !bytes.Equal(tlsMat.CertPEM, certA2) {
		t.Fatal("expected Restore to promote renewed cert A2 instead of stale pre-switch A1")
	}
	onDisk, err := os.ReadFile(filepath.Join(dir, "cert.pem"))
	if err != nil || !bytes.Equal(onDisk, certA2) {
		t.Fatalf("expected disk cert.pem to hold A2 after rollback promotion, err=%v", err)
	}

	// Verify real TLS handshake with restored TLSCert() presents acmea2-renewed.example.com (A2).
	pair, err := tls.X509KeyPair(tlsMat.CertPEM, tlsMat.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr == nil {
			defer c.Close()
			if tc, ok := c.(*tls.Conn); ok {
				_ = tc.Handshake()
			}
		}
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName
	_ = conn.Close()
	if cn != "acmea2-renewed.example.com" {
		t.Fatalf("TLS handshake CN = %q, want acmea2-renewed.example.com", cn)
	}
}
