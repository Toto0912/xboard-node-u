package cert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/cedar2025/xboard-node/internal/cert/dnsproviders"
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/nlog"
)

// Manager handles TLS certificate lifecycle.
//
// Supported modes (CertConfig.CertMode):
//   - "http"    — ACME HTTP-01 challenge via certmagic (needs port 80).
//   - "dns"     — ACME DNS-01 challenge via certmagic + libdns provider.
//   - "self"    — Self-signed certificate generated in memory.
//   - "file"    — User-provided certificate and key file paths (CertFile/KeyFile).
//   - "content" — Certificate and key PEM content pushed from the panel.
//   - "none"    — No TLS. The node will handle plain connections.
//
// All modes normalise to in-memory PEM. Kernels never see file paths —
// they always receive PEM bytes via TLSCert().
//
// Priority Logic (when CertMode is empty):
//
//  1. "http" (if auto_tls is true)
//  2. "content" (if both CertContent and KeyContent are provided)
//  3. "file" (if both CertFile and KeyFile paths are provided)
//  4. "none" (default)
type Manager struct {
	mu  sync.Mutex
	cfg config.CertConfig

	// Atomic so the ACME renewal goroutine can swap without racing readers.
	mat atomic.Pointer[certMaterial]

	magic           *certmagic.Config
	renewalSeq      uint64
	ackedRenewal    uint64
	renewCh         chan struct{}
	acmeStarted     bool
	acmeFingerprint string
	acmeCancel      context.CancelFunc
	activeACME      *acmeRuntime
	tx              *certTransaction
	ownedPersisted  bool
	ownedCertDigest [32]byte
	ownedKeyDigest  [32]byte
}

type acmeRuntime struct {
	magic       *certmagic.Config
	cache       *certmagic.Cache
	cancel      context.CancelFunc
	fingerprint string
}

// certTransaction owns the before-image until the kernel accepts the candidate.
type certTransaction struct {
	oldCfg        config.CertConfig
	oldMat        *certMaterial
	oldRuntime    *acmeRuntime
	oldRenewal    *certMaterial
	activeRenewed bool
}

func (r *acmeRuntime) stop() {
	if r == nil {
		return
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if r.cache != nil {
		r.cache.Stop()
		r.cache = nil
	}
	r.magic = nil
}

func (m *Manager) syncACMEFields() {
	if m.activeACME != nil {
		m.magic = m.activeACME.magic
		m.acmeCancel = m.activeACME.cancel
		m.acmeFingerprint = m.activeACME.fingerprint
		m.acmeStarted = true
	} else {
		m.magic = nil
		m.acmeCancel = nil
		m.acmeFingerprint = ""
		m.acmeStarted = false
	}
}

// certMaterial is an immutable snapshot of PEM-encoded cert + key.
type certMaterial struct {
	certPEM []byte
	keyPEM  []byte
}

// NewManager creates a certificate manager.
func NewManager(cfg config.CertConfig) *Manager {
	return &Manager{
		cfg:     cfg,
		renewCh: make(chan struct{}, 1),
	}
}

// storePEM atomically swaps the in-memory cert material and persists to disk
// (unless deferred during an uncommitted Reconfigure until Commit).
func (m *Manager) storePEM(cert, key []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mat.Store(&certMaterial{certPEM: cert, keyPEM: key})
	if m.tx == nil {
		m.persistActivePEMLocked(cert, key)
	}
}

func (m *Manager) persistActivePEMLocked(cert, key []byte) {
	if resolveModeFor(m.cfg) == "self" {
		m.persistSelfPEMLocked(cert, key)
		return
	}
	m.persistPEMLocked(cert, key)
}

func normalizeSelfDomain(domain string) string {
	d := strings.TrimSpace(domain)
	if d == "" {
		return "localhost"
	}
	return d
}

func selfFileNames(domain string) (string, string) {
	sum := sha256.Sum256([]byte(strings.ToLower(normalizeSelfDomain(domain))))
	tag := hex.EncodeToString(sum[:6])
	return fmt.Sprintf("self-%s-cert.pem", tag), fmt.Sprintf("self-%s-key.pem", tag)
}

func certMatchesSelfDomain(certPEM []byte, domain string) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	// Require self-signed leaf so CA-issued or unrelated certs are never treated as local self certs.
	if !bytes.Equal(leaf.RawIssuer, leaf.RawSubject) {
		return false
	}
	return leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature) == nil &&
		certValidFor(leaf, normalizeSelfDomain(domain))
}

func (m *Manager) persistSelfPEMLocked(cert, key []byte) {
	if err := writeSelfPEM(m.cfg.CertDir, m.cfg.Domain, cert, key); err != nil {
		nlog.Core().Warn("cert: failed to persist self-signed pair", "dir", m.cfg.CertDir, "error", err)
	}
}

func (m *Manager) removePersistedPEMLocked() {
	dir := m.cfg.CertDir
	if dir == "" || !m.ownedPersisted {
		return
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	diskCert, certErr := os.ReadFile(certPath)
	diskKey, keyErr := os.ReadFile(keyPath)
	if certErr != nil || keyErr != nil {
		return
	}
	if sha256.Sum256(diskCert) != m.ownedCertDigest || sha256.Sum256(diskKey) != m.ownedKeyDigest {
		m.ownedPersisted = false
		return
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	m.ownedPersisted = false
}

// Commit finalizes a successful Reconfigure by stopping any superseded ACME
// runtime and persisting the active PEM material to cert_dir for restart recovery.
func (m *Manager) Commit() {
	m.mu.Lock()
	var oldACME *acmeRuntime
	tx := m.tx
	m.tx = nil
	if tx != nil && tx.oldRuntime != m.activeACME {
		oldACME = tx.oldRuntime
		m.ackedRenewal = m.renewalSeq
	}
	if mat := m.mat.Load(); mat != nil && len(mat.certPEM) > 0 && len(mat.keyPEM) > 0 {
		m.persistActivePEMLocked(mat.certPEM, mat.keyPEM)
	} else if resolveModeFor(m.cfg) == "none" && m.ownedPersisted {
		m.removePersistedPEMLocked()
	}
	if tx != nil && tx.activeRenewed {
		m.notifyRenewedLocked()
	}
	m.mu.Unlock()
	oldACME.stop()
}

// persistPEMLocked writes cert/key to cert_dir for restart recovery.
// Caller must hold m.mu. Errors are logged but not returned.
func (m *Manager) persistPEMLocked(cert, key []byte) {
	dir := m.cfg.CertDir
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		nlog.Core().Warn("cert: failed to create cert_dir", "dir", dir, "error", err)
		return
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := atomicWriteFile(certPath, cert, 0o644); err != nil {
		nlog.Core().Warn("cert: failed to persist cert", "path", certPath, "error", err)
		return
	}
	if err := atomicWriteFile(keyPath, key, 0o600); err != nil {
		nlog.Core().Warn("cert: failed to persist key", "path", keyPath, "error", err)
		return
	}
	m.ownedPersisted = true
	m.ownedCertDigest = sha256.Sum256(cert)
	m.ownedKeyDigest = sha256.Sum256(key)
}

// atomicWriteFile writes data to a temp file and renames, preventing partial reads.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) loadPersistedFrom(certName, keyName string, markOwned bool) bool {
	m.mu.Lock()
	dir := m.cfg.CertDir
	m.mu.Unlock()
	if dir == "" {
		return false
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, certName))
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyName))
	if err != nil {
		return false
	}
	if err := validateKeyPair(certPEM, keyPEM); err != nil {
		nlog.Core().Warn("cert: persisted cert invalid, will regenerate", "error", err)
		return false
	}
	m.mu.Lock()
	// Store in memory only (don't re-persist what we just read).
	m.mat.Store(&certMaterial{certPEM: certPEM, keyPEM: keyPEM})
	if markOwned {
		m.ownedPersisted = true
		m.ownedCertDigest = sha256.Sum256(certPEM)
		m.ownedKeyDigest = sha256.Sum256(keyPEM)
	}
	m.mu.Unlock()
	nlog.Core().Info("cert: loaded persisted certificate from disk", "dir", dir)
	return true
}

// loadPersistedPEM loads previously persisted cert material from cert_dir.
// Returns true if valid material was loaded.
func (m *Manager) loadPersistedPEM() bool {
	return m.loadPersistedFrom("cert.pem", "key.pem", true)
}

// loadPersistedSelfPEM loads previously persisted self-signed cert material for
// domain, migrating matching legacy self-cert.pem or v2.3 cert.pem copies if found.
func (m *Manager) loadPersistedSelfPEM(domain string) (bool, error) {
	m.mu.Lock()
	dir, legacy := m.cfg.CertDir, m.cfg.LegacyCertDir
	m.mu.Unlock()

	certName, keyName := selfFileNames(domain)
	names := [][2]string{{certName, keyName}, {"self-cert.pem", "self-key.pem"}, {"cert.pem", "key.pem"}}
	for _, root := range []string{dir, legacy} {
		if root == "" {
			continue
		}
		for _, pair := range names {
			certPEM, err := os.ReadFile(filepath.Join(root, pair[0]))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			keyPEM, err := os.ReadFile(filepath.Join(root, pair[1]))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			if validateKeyPair(certPEM, keyPEM) != nil || !certMatchesSelfDomain(certPEM, domain) {
				continue
			}
			m.mu.Lock()
			if (root != dir || pair[0] != certName) && m.tx == nil {
				err = writeSelfPEM(dir, domain, certPEM, keyPEM)
			}
			if err == nil {
				m.mat.Store(&certMaterial{certPEM: certPEM, keyPEM: keyPEM})
			}
			m.mu.Unlock()
			return err == nil, err
		}
	}
	return false, nil
}

// Reconfigure applies a new cert configuration at runtime (e.g. from panel push).
// Disk persistence and old ACME teardown wait until Commit so failed rollouts can
// Restore cleanly. Returns true when the PEM material changed (caller should restart kernel).
func (m *Manager) Reconfigure(ctx context.Context, newCfg config.CertConfig) (bool, error) {
	m.mu.Lock()
	if m.tx != nil {
		m.mu.Unlock()
		return false, fmt.Errorf("certificate reconfiguration is already pending")
	}
	if newCfg.CertDir == "" {
		newCfg.CertDir = m.cfg.CertDir
	}
	// LegacyCertDir is local migration state, never a remote override.
	newCfg.LegacyCertDir = m.cfg.LegacyCertDir
	oldTLS := m.TLSCert()
	m.tx = &certTransaction{oldCfg: m.cfg, oldMat: m.mat.Load(), oldRuntime: m.activeACME}
	if m.activeACME != nil {
		newMode := resolveModeFor(newCfg)
		if (newMode != "http" && newMode != "dns") || acmeFingerprint(newCfg) != m.activeACME.fingerprint {
			m.activeACME = nil
			m.syncACMEFields()
		}
	}
	m.cfg = newCfg
	m.mu.Unlock()

	if err := m.Start(ctx); err != nil {
		m.Rollback()
		return false, fmt.Errorf("cert reconfigure: %w", err)
	}
	return !pemEqual(oldTLS, m.TLSCert()), nil
}

// Restore is idempotent. The before-image belongs to the manager so that a
// service snapshot taken before a renewal cannot overwrite newer material.
func (m *Manager) Restore(_ config.CertConfig, _ kernel.TLSCert) {
	m.Rollback()
}

// Rollback restores the current transaction's before-image and is a no-op
// once the transaction was already rolled back or committed.
func (m *Manager) Rollback() {
	m.mu.Lock()
	tx := m.tx
	if tx == nil {
		m.mu.Unlock()
		return
	}
	m.tx = nil
	m.cfg = tx.oldCfg
	var discard *acmeRuntime
	mat := tx.oldMat
	renewed := false
	if tx.oldRuntime != nil && tx.oldRuntime == m.activeACME {
		if tx.activeRenewed {
			mat = m.mat.Load()
			renewed = true
		}
	} else {
		discard = m.activeACME
		m.activeACME = tx.oldRuntime
		m.syncACMEFields()
		if tx.oldRenewal != nil {
			mat = tx.oldRenewal
			renewed = true
		}
	}
	m.mat.Store(mat)
	if renewed {
		m.persistActivePEMLocked(mat.certPEM, mat.keyPEM)
		m.notifyRenewedLocked()
	}
	m.mu.Unlock()
	discard.stop()
}

// acmeFingerprint produces a stable string capturing every config field that
// requires re-running ACME when changed.
func acmeFingerprint(cfg config.CertConfig) string {
	keys := make([]string, 0, len(cfg.DNSEnv))
	for k := range cfg.DNSEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(strings.ToLower(strings.TrimSpace(cfg.CertMode)))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.Domain))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.Email))
	b.WriteByte('|')
	b.WriteString(strings.TrimSpace(cfg.DNSProvider))
	b.WriteByte('|')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(cfg.DNSEnv[k])
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// resolveModeFor mirrors (*Manager).resolveMode() for an arbitrary cfg, used
// by Reconfigure to decide tear-down without mutating manager state.
func resolveModeFor(cfg config.CertConfig) string {
	mode := strings.ToLower(strings.TrimSpace(cfg.CertMode))
	if mode != "" {
		return mode
	}
	if cfg.AutoTLS {
		return "http"
	}
	if cfg.CertContent != "" && cfg.KeyContent != "" {
		return "content"
	}
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		return "file"
	}
	return "none"
}

// resolveMode returns the effective cert mode, handling backward compat for auto_tls.
func (m *Manager) resolveMode() string {
	mode := strings.ToLower(strings.TrimSpace(m.cfg.CertMode))
	if mode != "" {
		return mode
	}
	// Backward compat: auto_tls: true → "http"
	if m.cfg.AutoTLS {
		return "http"
	}
	// Inline PEM content provided → "content"
	if m.cfg.CertContent != "" && m.cfg.KeyContent != "" {
		return "content"
	}
	// If cert/key file paths are provided → "file"
	if m.cfg.CertFile != "" && m.cfg.KeyFile != "" {
		return "file"
	}
	return "none"
}

func (m *Manager) HasCert() bool {
	mat := m.mat.Load()
	return mat != nil && len(mat.certPEM) > 0 && len(mat.keyPEM) > 0
}
func (m *Manager) notifyRenewed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyRenewedLocked()
}

func (m *Manager) notifyRenewedLocked() {
	m.renewalSeq++
	if m.renewCh != nil {
		select {
		case m.renewCh <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) MarkRenewed()               { m.notifyRenewed() }
func (m *Manager) RenewedCh() <-chan struct{} { return m.renewCh }

// PendingRenewalSnapshot binds an immutable certificate to its exact renewal.
func (m *Manager) PendingRenewalSnapshot() (kernel.TLSCert, uint64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.TLSCert(), m.renewalSeq, m.tx == nil && m.ackedRenewal < m.renewalSeq
}

func (m *Manager) HasPendingRenewal() bool {
	_, _, pending := m.PendingRenewalSnapshot()
	return pending
}

func (m *Manager) AckRenewedThrough(seq uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq > m.ackedRenewal && seq <= m.renewalSeq {
		m.ackedRenewal = seq
	}
}

func (m *Manager) AckRenewed() {
	m.CertRenewed()
}

func (m *Manager) CertRenewed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tx != nil || m.ackedRenewal == m.renewalSeq {
		return false
	}
	m.ackedRenewal = m.renewalSeq
	return true
}

// TLSCert returns the current PEM material as a kernel.TLSCert.
func (m *Manager) TLSCert() kernel.TLSCert {
	mat := m.mat.Load()
	if mat == nil {
		return kernel.TLSCert{}
	}
	return kernel.TLSCert{CertPEM: mat.certPEM, KeyPEM: mat.keyPEM}
}

// pemEqual reports whether two TLSCert values carry identical PEM content.
func pemEqual(a, b kernel.TLSCert) bool {
	return string(a.CertPEM) == string(b.CertPEM) && string(a.KeyPEM) == string(b.KeyPEM)
}

// Start initializes the cert manager based on the resolved mode.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	mode := m.resolveMode()
	m.mu.Unlock()

	switch mode {
	case "none", "":
		m.mu.Lock()
		m.mat.Store(nil)
		m.mu.Unlock()
		return nil
	case "file":
		return m.startFile()
	case "self":
		return m.startSelfSigned()
	case "content":
		return m.startContent()
	case "http":
		return m.startACME(ctx, nil)
	case "dns":
		solver, err := m.buildDNSSolver()
		if err != nil {
			return fmt.Errorf("build dns solver: %w", err)
		}
		return m.startACME(ctx, solver)
	default:
		return fmt.Errorf("unknown cert_mode: %q (supported: http, dns, self, file, content, none)", mode)
	}
}

// Stop tears down any active or pending ACME certmagic runtimes and caches.
func (m *Manager) Stop() {
	m.mu.Lock()
	active := m.activeACME
	var old *acmeRuntime
	if m.tx != nil && m.tx.oldRuntime != active {
		old = m.tx.oldRuntime
	}
	m.tx = nil
	m.activeACME = nil
	m.syncACMEFields()
	m.mat.Store(nil)
	m.mu.Unlock()
	active.stop()
	old.stop()
}

// ─── Mode: file ────────────────────────────────────────────────────────────

// PreviewMaterial reads static certificates without changing the manager or its
// on-disk copies. Automatic modes acquire material during normal startup.
func PreviewMaterial(cfg config.CertConfig) (material kernel.TLSCert, configured bool, err error) {
	switch mode := resolveModeFor(cfg); mode {
	case "none":
		return material, false, nil
	case "self":
		return material, true, nil
	case "http", "dns":
		if cfg.Domain == "" {
			return material, false, fmt.Errorf("cert.domain is required for ACME modes")
		}
		return material, true, nil
	case "file":
		material, err = readKeyPair(cfg.CertFile, cfg.KeyFile)
	case "content":
		if cfg.CertContent == "" || cfg.KeyContent == "" {
			material, err = readKeyPair(filepath.Join(cfg.CertDir, "cert.pem"), filepath.Join(cfg.CertDir, "key.pem"))
		} else {
			material = kernel.TLSCert{CertPEM: []byte(cfg.CertContent), KeyPEM: []byte(cfg.KeyContent)}
			err = validateKeyPair(material.CertPEM, material.KeyPEM)
		}
	default:
		err = fmt.Errorf("unsupported cert_mode %q", mode)
	}
	return material, err == nil, err
}

func readKeyPair(certFile, keyFile string) (material kernel.TLSCert, err error) {
	if material.CertPEM, err = os.ReadFile(certFile); err != nil {
		return material, fmt.Errorf("cert file: %w", err)
	}
	if material.KeyPEM, err = os.ReadFile(keyFile); err != nil {
		return material, fmt.Errorf("key file: %w", err)
	}
	return material, validateKeyPair(material.CertPEM, material.KeyPEM)
}

func (m *Manager) startFile() error {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	material, err := readKeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	m.storePEM(material.CertPEM, material.KeyPEM)
	nlog.Core().Debug("TLS certificate loaded from files", "cert", cfg.CertFile, "key", cfg.KeyFile)
	return nil
}

// ─── Mode: self ────────────────────────────────────────────────────────────

func (m *Manager) startSelfSigned() error {
	m.mu.Lock()
	domain := normalizeSelfDomain(m.cfg.Domain)
	m.mu.Unlock()

	// Reuse persisted self-signed cert for this domain if available and valid.
	if found, err := m.loadPersistedSelfPEM(domain); found || err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: domain},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	// Add SANs
	if ip := net.ParseIP(domain); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{domain}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	m.storePEM(certPEM, keyPEM)
	nlog.Core().Info("self-signed certificate generated (in-memory)", "domain", domain, "valid_years", 10)
	return nil
}

// ─── Mode: content ────────────────────────────────────────────────────────

// startContent stores panel-supplied PEM strings in memory for inline use.
func (m *Manager) startContent() error {
	m.mu.Lock()
	certContent := m.cfg.CertContent
	keyContent := m.cfg.KeyContent
	m.mu.Unlock()

	if certContent == "" || keyContent == "" {
		// No fresh content from panel — try previously persisted material.
		if m.loadPersistedPEM() {
			return nil
		}
		return fmt.Errorf("cert_mode 'content' requires both cert_content and key_content")
	}
	certPEM := []byte(certContent)
	keyPEM := []byte(keyContent)
	if err := validateKeyPair(certPEM, keyPEM); err != nil {
		return fmt.Errorf("invalid certificate content: %w", err)
	}
	m.storePEM(certPEM, keyPEM)
	nlog.Core().Info("TLS certificate loaded from panel content (in-memory)")
	return nil
}

// ─── Mode: http / dns (ACME) ──────────────────────────────────────────────

func (m *Manager) startACME(ctx context.Context, dnsSolver *certmagic.DNS01Solver) error {
	m.mu.Lock()
	// Idempotent for unchanged config: Reconfigure already detached stale ACME state.
	if m.acmeStarted && m.HasCert() {
		m.mu.Unlock()
		return nil
	}
	domain := m.cfg.Domain
	certDir := m.cfg.CertDir
	legacyDir := m.cfg.LegacyCertDir
	email := m.cfg.Email
	httpPort := m.cfg.HTTPPort
	fp := acmeFingerprint(m.cfg)
	m.mu.Unlock()

	if domain == "" {
		return fmt.Errorf("cert.domain is required for ACME modes (http/dns)")
	}

	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	storage := &certmagic.FileStorage{Path: certDir}
	rt := &acmeRuntime{fingerprint: fp}

	var magic *certmagic.Config
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(_ certmagic.Certificate) (*certmagic.Config, error) {
			return magic, nil
		},
	})
	succeeded := false
	defer func() {
		if !succeeded {
			cache.Stop()
		}
	}()

	magic = certmagic.New(cache, certmagic.Config{
		Storage: storage,
		OnEvent: func(evtCtx context.Context, event string, data map[string]any) error {
			// Only react to renewals; initial load happens explicitly after ObtainCertSync.
			if event != "cert_obtained" {
				return nil
			}
			if renewed, _ := data["renewal"].(bool); !renewed {
				return nil
			}
			issuerKey, _ := data["issuer"].(string)
			if issuerKey == "" {
				return nil
			}
			m.handleACMERenewal(evtCtx, rt, storage, issuerKey, domain)
			return nil
		},
	})

	issuer := certmagic.ACMEIssuer{
		CA:    certmagic.LetsEncryptProductionCA,
		Email: email,
	}

	if dnsSolver != nil {
		// DNS-01 mode: no HTTP port needed, supports wildcards.
		issuer.DNS01Solver = dnsSolver
		issuer.DisableHTTPChallenge = true
		issuer.DisableTLSALPNChallenge = true
	} else {
		// HTTP-01 mode.
		if httpPort == 0 {
			httpPort = 80
		}
		issuer.AltHTTPPort = httpPort
		issuer.DisableTLSALPNChallenge = true
	}

	magic.Issuers = []certmagic.Issuer{
		certmagic.NewACMEIssuer(magic, issuer),
	}

	// Derived ctx so Reconfigure/Restore/Stop can cancel ACME background goroutines.
	acmeCtx, acmeCancel := context.WithCancel(ctx)

	issuerKey := magic.Issuers[0].IssuerKey()
	if legacyDir != "" {
		if err := migrateACMESite(acmeCtx, storage, legacyDir, issuerKey, domain); err != nil {
			acmeCancel()
			return fmt.Errorf("migrate ACME site: %w", err)
		}
	}
	if err := magic.ObtainCertSync(acmeCtx, domain); err != nil {
		acmeCancel()
		return fmt.Errorf("obtain certificate: %w", err)
	}

	if err := m.loadPEMFromStorage(acmeCtx, storage, issuerKey, domain); err != nil {
		acmeCancel()
		return fmt.Errorf("load cert from storage: %w", err)
	}

	// ManageAsync dispatches background jobs. Publish their runtime before
	// releasing the lock used by renewal callbacks to identify their owner.
	m.mu.Lock()
	if err := magic.ManageAsync(acmeCtx, []string{domain}); err != nil {
		m.mu.Unlock()
		acmeCancel()
		return fmt.Errorf("start cert manager: %w", err)
	}

	succeeded = true
	rt.magic = magic
	rt.cache = cache
	rt.cancel = acmeCancel

	m.activeACME = rt
	m.syncACMEFields()
	m.mu.Unlock()
	return nil
}

func validateKeyPair(certPEM, keyPEM []byte) error {
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return err
	}
	return nil
}

// ─── DNS Provider Factory ──────────────────────────────────────────────────

// buildDNSSolver builds a certmagic DNS01Solver from the configured provider.
func (m *Manager) buildDNSSolver() (*certmagic.DNS01Solver, error) {
	provider, err := m.newDNSProvider()
	if err != nil {
		return nil, err
	}
	return &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{DNSProvider: provider}}, nil
}

func (m *Manager) newDNSProvider() (certmagic.DNSProvider, error) {
	m.mu.Lock()
	name := strings.TrimSpace(m.cfg.DNSProvider)
	env := m.cfg.DNSEnv
	m.mu.Unlock()
	if name == "" {
		return nil, fmt.Errorf("dns_provider is required for cert_mode=dns")
	}
	if env == nil {
		env = map[string]string{}
	}
	p, ok := dnsproviders.Get(name)
	if !ok {
		return nil, fmt.Errorf("unsupported dns_provider: %q (supported: %s)",
			name, strings.Join(dnsproviders.CanonicalNames(), ", "))
	}
	return p.Build(env)
}

// ─── Helpers ───────────────────────────────────────────────────────────────

func readPEMFromStorage(ctx context.Context, storage certmagic.Storage, issuerKey, domain string) ([]byte, []byte, error) {
	if issuerKey == "" || domain == "" {
		return nil, nil, fmt.Errorf("loadPEMFromStorage: issuerKey and domain are required")
	}
	certKey := certmagic.StorageKeys.SiteCert(issuerKey, domain)
	keyKey := certmagic.StorageKeys.SitePrivateKey(issuerKey, domain)

	certPEM, err := storage.Load(ctx, certKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load cert %q: %w", certKey, err)
	}
	keyPEM, err := storage.Load(ctx, keyKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load key %q: %w", keyKey, err)
	}
	if err := validateKeyPair(certPEM, keyPEM); err != nil {
		return nil, nil, fmt.Errorf("invalid cert/key pair on disk: %w", err)
	}
	return certPEM, keyPEM, nil
}

// loadPEMFromStorage reads cert + key for domain from certmagic storage into
// the in-memory cache. Single entry point for initial load.
func (m *Manager) loadPEMFromStorage(ctx context.Context, storage certmagic.Storage, issuerKey, domain string) error {
	certPEM, keyPEM, err := readPEMFromStorage(ctx, storage, issuerKey, domain)
	if err != nil {
		return err
	}
	m.storePEM(certPEM, keyPEM)
	return nil
}

// handleACMERenewal verifies under lock that the firing ACME runtime is still
// the active one before publishing renewed PEM material or notifying reload.
func (m *Manager) handleACMERenewal(evtCtx context.Context, rt *acmeRuntime, storage certmagic.Storage, issuerKey, domain string) bool {
	certPEM, keyPEM, err := readPEMFromStorage(evtCtx, storage, issuerKey, domain)
	if err != nil {
		nlog.Core().Error("failed to reload cert after renewal", "domain", domain, "error", err)
		return false
	}
	m.mu.Lock()
	if m.activeACME == rt {
		m.mat.Store(&certMaterial{certPEM: certPEM, keyPEM: keyPEM})
		if m.tx != nil {
			m.tx.activeRenewed = true
		} else {
			m.persistActivePEMLocked(certPEM, keyPEM)
			m.notifyRenewedLocked()
		}
		m.mu.Unlock()
		nlog.Core().Info("TLS certificate reloaded after renewal", "domain", domain)
		return true
	}
	if m.tx != nil && m.tx.oldRuntime == rt {
		// Candidate switch has not committed yet; hold renewal so Restore can apply A2.
		m.tx.oldRenewal = &certMaterial{certPEM: certPEM, keyPEM: keyPEM}
	}
	m.mu.Unlock()
	return false
}
