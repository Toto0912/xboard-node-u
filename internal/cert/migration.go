package cert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

func certValidFor(leaf *x509.Certificate, domain string) bool {
	now := time.Now()
	return !now.Before(leaf.NotBefore) && !now.After(leaf.NotAfter) && leaf.VerifyHostname(domain) == nil
}

func writeSelfPEM(dir, domain string, cert, key []byte) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	certName, keyName := selfFileNames(domain)
	if err := atomicWriteFile(filepath.Join(dir, keyName), key, 0o600); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, certName), cert, 0o644)
}

var errInvalidSite = errors.New("unusable ACME site")

type siteBundle struct {
	cert, key, meta []byte
}

func loadSite(ctx context.Context, store *certmagic.FileStorage, issuer, domain string) (*siteBundle, error) {
	keys := certmagic.StorageKeys
	cert, err := store.Load(ctx, keys.SiteCert(issuer, domain))
	if err != nil {
		return nil, err
	}
	key, err := store.Load(ctx, keys.SitePrivateKey(issuer, domain))
	if err != nil {
		return nil, err
	}
	meta, err := store.Load(ctx, keys.SiteMeta(issuer, domain))
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("%w: cert/key do not match", errInvalidSite)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !certValidFor(leaf, domain) {
		return nil, fmt.Errorf("%w: certificate not valid for %s", errInvalidSite, domain)
	}
	var resource certmagic.CertificateResource
	if err := json.Unmarshal(meta, &resource); err == nil {
		for _, name := range resource.SANs {
			if strings.EqualFold(name, domain) {
				return &siteBundle{cert, key, meta}, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: missing or invalid site metadata for %s", errInvalidSite, domain)
}

// Publish metadata last. CertMagic treats a site as present only when all three
// keys exist; an interrupted migration can always be retried from the read-only source.
func migrateACMESite(ctx context.Context, dst *certmagic.FileStorage, legacyDir, issuer, domain string) error {
	// Use the same per-domain lock as the pinned CertMagic obtain and renew paths.
	lock := "issue_cert_" + domain
	if err := dst.Lock(ctx, lock); err != nil {
		return err
	}
	defer dst.Unlock(ctx, lock)
	_, dstErr := loadSite(ctx, dst, issuer, domain)
	if dstErr == nil {
		return nil
	}
	if !errors.Is(dstErr, os.ErrNotExist) && !errors.Is(dstErr, errInvalidSite) {
		return dstErr
	}
	src := &certmagic.FileStorage{Path: legacyDir}
	bundle, err := loadSite(ctx, src, issuer, domain)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errInvalidSite) {
		return err
	}
	keys := certmagic.StorageKeys
	meta := keys.SiteMeta(issuer, domain)
	if bundle == nil {
		if errors.Is(dstErr, errInvalidSite) {
			// Keep the old material for diagnosis, but prevent ObtainCertSync
			// from skipping issuance merely because the corrupted triple exists.
			metaPath := dst.Filename(meta)
			if err := os.Rename(metaPath, metaPath+".invalid"); err != nil {
				return err
			}
		}
		return nil
	}
	if err := os.Remove(dst.Filename(meta)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := dst.Store(ctx, keys.SitePrivateKey(issuer, domain), bundle.key); err != nil {
		return err
	}
	if err := dst.Store(ctx, keys.SiteCert(issuer, domain), bundle.cert); err != nil {
		return err
	}
	if err := dst.Store(ctx, meta, bundle.meta); err != nil {
		return err
	}
	_, err = loadSite(ctx, dst, issuer, domain)
	return err
}
