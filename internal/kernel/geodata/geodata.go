package geodata

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cedar2025/xboard-node/internal/nlog"
	"golang.org/x/sync/singleflight"
)

const (
	singboxGeoIPURL   = "https://github.com/SagerNet/sing-geoip/releases/latest/download/geoip.db"
	singboxGeoSiteURL = "https://github.com/SagerNet/sing-geosite/releases/latest/download/geosite.db"
	xrayGeoIPURL      = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat"
	xrayGeoSiteURL    = "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat"
)

var downloads singleflight.Group

var httpClient = &http.Client{Timeout: 10 * time.Minute}

func EnsureContext(ctx context.Context, dir string, needGeoIP, needGeoSite bool, kernelType string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create geo_data_dir %q: %w", dir, err)
	}

	type entry struct {
		name string
		url  string
	}

	var files []entry
	if kernelType == "singbox" {
		if needGeoIP {
			files = append(files, entry{"geoip.db", singboxGeoIPURL})
		}
		if needGeoSite {
			files = append(files, entry{"geosite.db", singboxGeoSiteURL})
		}
	} else {
		if needGeoIP {
			files = append(files, entry{"geoip.dat", xrayGeoIPURL})
		}
		if needGeoSite {
			files = append(files, entry{"geosite.dat", xrayGeoSiteURL})
		}
	}

	var firstErr error
	for _, f := range files {
		if err := ensureFile(ctx, dir, f.name, f.url); err != nil {
			nlog.Core().Warn("geo database auto-download failed", "file", f.name, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func ensureFile(ctx context.Context, dir, name, url string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	result := downloads.DoChan(dst, func() (any, error) {
		if info, err := os.Stat(dst); err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("geo database is not a file: %s", dst)
			}
			if info.Size() > 0 {
				return nil, nil
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		nlog.Core().Info("geo database missing, downloading automatically", "file", name, "url", url)
		if err := atomicDownload(dst, url); err != nil {
			return nil, err
		}
		if fi, err := os.Stat(dst); err == nil {
			nlog.Core().Info("geo database ready", "file", name, "size_kb", fi.Size()/1024)
		}
		return nil, nil
	})
	// A cancelling node no longer waits. The bounded shared download remains
	// available to other nodes which are still preparing the same file.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case completed := <-result:
		return completed.Err
	}
}

func atomicDownload(dst, url string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("http get %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status from %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp(filepath.Dir(dst), ".geodata-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	n, err := io.Copy(f, resp.Body)
	if err == nil && n == 0 {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = f.Chmod(0644)
	}
	if err != nil {
		f.Close()
		return fmt.Errorf("write geo database: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename to destination: %w", err)
	}
	return nil
}
