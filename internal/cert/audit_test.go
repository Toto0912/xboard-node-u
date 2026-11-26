package cert

import (
	"bytes"
	"context"
	"github.com/cedar2025/xboard-node/internal/config"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedFileCertificatesSurviveOverrides(t *testing.T) {
	for _, alias := range []bool{false, true} {
		dir := t.TempDir()
		cache := dir
		if alias {
			cache = filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(dir, cache); err != nil {
				t.Fatal(err)
			}
		}
		crt, key := generateLongLivedPair(t, "original.test")
		cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		for path, data := range map[string][]byte{cf: crt, kf: key} {
			if err := os.WriteFile(path, data, 0400); err != nil {
				t.Fatal(err)
			}
		}
		cfg := config.CertConfig{CertMode: "file", CertDir: cache, CertFile: cf, KeyFile: kf}
		m := NewManager(cfg)
		ctx := context.Background()
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		b, bk := generateLongLivedPair(t, "new.test")
		for _, mode := range []string{"content", "none"} {
			if _, err := m.Reconfigure(ctx, config.CertConfig{CertMode: mode, CertContent: string(b), KeyContent: string(bk), CertDir: cache}); err != nil {
				t.Fatal(err)
			}
			m.Commit()
		}
		m.Stop()
		for path, want := range map[string][]byte{cf: crt, kf: key} {
			got, err := os.ReadFile(path)
			info, e := os.Stat(path)
			if err != nil || e != nil || !bytes.Equal(got, want) || info.Mode().Perm() != 0400 {
				t.Fatalf("changed %s", filepath.Base(path))
			}
		}
		m = NewManager(cfg)
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		m.Stop()
	}
}
func TestACMEChangeIdentityIncludesPortAndDirectory(t *testing.T) {
	c := config.CertConfig{Domain: "test", HTTPPort: 80, CertDir: "a"}
	f := acmeFingerprint(c)
	c.HTTPPort++
	if f == acmeFingerprint(c) {
		t.Fatal("port ignored")
	}
	c.HTTPPort--
	c.CertDir = "b"
	if f == acmeFingerprint(c) {
		t.Fatal("storage ignored")
	}
}
