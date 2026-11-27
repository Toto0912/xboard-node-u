package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cedar2025/xboard-node/internal/download"
)

var releaseTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

func pinnedRelease(client *http.Client, base, version string) (string, error) {
	if version == "latest" {
		resp, err := client.Head(base + "/latest")
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		repo, err := url.Parse(base)
		if err != nil {
			return "", err
		}
		end := resp.Request.URL
		tagPath := repo.Path + "/tag/"
		if resp.StatusCode != http.StatusOK || end.Scheme != repo.Scheme || !strings.EqualFold(end.Host, repo.Host) ||
			len(end.Path) <= len(tagPath) || !strings.EqualFold(end.Path[:len(tagPath)], tagPath) {
			return "", errors.New("latest release did not resolve to a tag in the configured repository")
		}
		version = end.Path[len(tagPath):]
	}
	if !releaseTag.MatchString(version) {
		return "", fmt.Errorf("invalid release tag %q", version)
	}
	return version, nil
}

func releaseFile(client *http.Client, address string) (io.ReadCloser, error) {
	resp, err := client.Get(address)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", address, resp.StatusCode)
	}
	return resp.Body, nil
}

func releaseDigests(client *http.Client, base string, assets []string) (map[string]string, error) {
	body, err := releaseFile(client, base+"/SHA256SUMS.txt")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	digests := make(map[string]string)
	scan := bufio.NewScanner(body)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if !slices.Contains(assets, name) {
			continue
		}
		hash, err := hex.DecodeString(fields[0])
		if err != nil || len(hash) != sha256.Size || digests[name] != "" {
			return nil, fmt.Errorf("invalid or duplicate SHA256 for %s", name)
		}
		digests[name] = strings.ToLower(fields[0])
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if len(digests) != len(assets) {
		return nil, errors.New("release checksum manifest does not cover all requested assets")
	}
	return digests, nil
}

func fetchChecked(client *http.Client, address, dest, digest string) error {
	body, err := releaseFile(client, address)
	if err != nil {
		return err
	}
	defer body.Close()
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), body)
	if err := errors.Join(copyErr, file.Close()); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("SHA256 mismatch for %s", filepath.Base(dest))
	}
	return nil
}

func serviceActiveState() (string, error) {
	out, err := exec.Command("systemctl", "show", "--property=ActiveState", "--value", serviceName).Output()
	if err != nil {
		return "", fmt.Errorf("read service state: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func restoreStoppedService(previous string) error {
	if err := runCommand("systemctl", "stop", serviceName); err != nil {
		return err
	}
	state, err := serviceActiveState()
	if err != nil {
		return err
	}
	if previous == "inactive" && state == "failed" {
		if err := runCommand("systemctl", "reset-failed", serviceName); err != nil {
			return err
		}
		state, err = serviceActiveState()
		if err != nil {
			return err
		}
	}
	if state != previous {
		return fmt.Errorf("restored service state is %s (expected %s)", state, previous)
	}
	return nil
}

// Type=simple may report a successful restart before configuration has been read.
// Keep both backups through the packaged unit's first RestartSec (5s).
func serviceStartIdentity() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState,MainPID,NRestarts", serviceName).Output()
	if err != nil {
		return "", fmt.Errorf("read service state: %w", err)
	}
	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = strings.TrimSpace(value)
		}
	}
	pid, err := strconv.Atoi(props["MainPID"])
	if props["ActiveState"] != "active" || err != nil || pid <= 0 {
		return "", fmt.Errorf("service did not stay active: state=%s pid=%s", props["ActiveState"], props["MainPID"])
	}
	return props["MainPID"] + ":" + props["NRestarts"], nil
}

func observeSystemdStart(window, interval time.Duration, read func() (string, error)) error {
	first, err := read()
	if err != nil {
		return err
	}
	until := time.Now().Add(window)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		<-tick.C
		current, err := read()
		if err != nil {
			return err
		}
		if current != first {
			return errors.New("service process changed during startup")
		}
		if !time.Now().Before(until) {
			return nil
		}
	}
}

// Require the installed service itself to survive restart even when another
// process responds on the configured address. HealthPort zero disables HTTP.
func observeInstalledStart(port int) error {
	if err := observeSystemdStart(6*time.Second, 500*time.Millisecond, serviceStartIdentity); err != nil {
		return err
	}
	if port <= 0 {
		return nil
	}
	identity, err := serviceStartIdentity()
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(24 * time.Second)
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		healthy := false
		if err == nil {
			resp.Body.Close()
			healthy = resp.StatusCode == http.StatusOK
		}
		state, err := serviceStartIdentity()
		if err != nil {
			return err
		}
		if state != identity {
			return errors.New("service process changed during health check")
		}
		if healthy {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check on port %d did not become ready", port)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func upgradeRelease(transport http.RoundTripper, repoBase, requested, arch, binDir, installerPath string, prepare func() error, record func(string, []byte) error, restart func(rollback bool) error) error {
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported architecture: %s", arch)
	}
	lock, err := lockInstallDir(binDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	client := download.Client(120*time.Second, transport)
	tag, err := pinnedRelease(client, repoBase, requested)
	if err != nil {
		return err
	}
	fmt.Printf("Downloading release %s for linux-%s...\n", tag, arch)
	release := repoBase + "/download/" + tag
	names := []string{"xboard-node", "xbctl"}
	assets := []string{"xboard-node-linux-" + arch, "xbctl-linux-" + arch}
	info, err := os.Stat(installerPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	refreshInstaller := err == nil
	if refreshInstaller {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("expected a regular installer: %s", installerPath)
		}
		assets = append(assets, "install.sh")
	}
	digests, err := releaseDigests(client, release, assets)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(binDir, ".xbctl-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for i, name := range names {
		if err := fetchChecked(client, release+"/"+assets[i], filepath.Join(stage, name), digests[assets[i]]); err != nil {
			return err
		}
	}
	var installer []byte
	if refreshInstaller {
		path := filepath.Join(stage, "install.sh")
		if err := fetchChecked(client, release+"/install.sh", path, digests["install.sh"]); err != nil {
			return err
		}
		installer, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	for i, name := range names {
		candidate := filepath.Join(stage, name)
		if err := os.Chmod(candidate, 0755); err != nil {
			return err
		}
		arg := []string{"-v", "version"}[i]
		out, err := exec.Command(candidate, arg).Output()
		fields := strings.Fields(string(out))
		if err != nil || len(fields) < 2 || fields[0] != name || fields[1] != tag {
			return fmt.Errorf("%s did not report the expected release %s", name, tag)
		}
	}

	if prepare != nil {
		if err := prepare(); err != nil {
			return err
		}
	}
	// Both candidates are verified before the first live file is changed.
	existed := make(map[string]bool)
	for _, name := range names {
		path := filepath.Join(binDir, name)
		if _, err := os.Stat(path); err == nil {
			if err := copyFile(path, path+".bak"); err != nil {
				return err
			}
			existed[name] = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	restore := func(changed []string) error {
		var failures []error
		for _, name := range changed {
			path := filepath.Join(binDir, name)
			if existed[name] {
				failures = append(failures, os.Rename(path+".bak", path))
			} else {
				failures = append(failures, os.Remove(path))
			}
		}
		return errors.Join(failures...)
	}
	for i, name := range names {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(binDir, name)); err != nil {
			return errors.Join(err, restore(names[:i]))
		}
	}
	applyErr := restart(false)
	if applyErr == nil && record != nil {
		applyErr = record(tag, installer)
	}
	if applyErr != nil {
		reason := fmt.Errorf("could not activate release %s: %w", tag, applyErr)
		if restoreErr := restore(names); restoreErr != nil {
			return errors.Join(reason, fmt.Errorf("rollback: %w", restoreErr))
		}
		if retryErr := restart(true); retryErr != nil {
			return errors.Join(reason, fmt.Errorf("restart after rollback: %w", retryErr))
		}
		return fmt.Errorf("%w (previous installation restored)", reason)
	}
	for _, name := range names {
		if existed[name] {
			os.Remove(filepath.Join(binDir, name) + ".bak")
		}
	}
	return nil
}
