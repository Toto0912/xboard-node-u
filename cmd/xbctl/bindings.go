package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cedar2025/xboard-node/internal/config"
	"gopkg.in/yaml.v3"
)

type bindingFile struct {
	path string
	data []byte
	mode os.FileMode
}

// Stage all replacements in their destination directories before publishing any.
// Retain the old files until the last rename, so a later rename error can roll back.
func commitBindingFiles(files []bindingFile) error {
	type change struct{ dst, staged, old string }
	var changes []change
	var temps []string
	defer func() {
		for _, name := range temps {
			_ = os.Remove(name)
		}
	}()
	seen := make(map[string]bool)
	for _, f := range files {
		dst, err := filepath.Abs(f.path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(dst)
		had := err == nil
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if had && info.Mode()&os.ModeSymlink != 0 {
			dst, err = filepath.EvalSymlinks(dst)
			if err != nil {
				return err
			}
			info, err = os.Stat(dst)
			if err != nil {
				return err
			}
		}
		if had && !info.Mode().IsRegular() {
			return fmt.Errorf("expected regular file: %s", dst)
		}
		if seen[dst] {
			return fmt.Errorf("duplicate binding output: %s", dst)
		}
		seen[dst] = true
		stage := func(data []byte, mode os.FileMode) (string, error) {
			tmp, err := os.CreateTemp(filepath.Dir(dst), ".xbctl-*")
			if err != nil {
				return "", err
			}
			temps = append(temps, tmp.Name())
			if err = tmp.Chmod(mode); err == nil {
				_, err = tmp.Write(data)
			}
			if err == nil {
				err = tmp.Sync()
			}
			return tmp.Name(), errors.Join(err, tmp.Close())
		}
		var old string
		if had {
			f.mode &= info.Mode().Perm()
			data, err := os.ReadFile(dst)
			if err != nil {
				return err
			}
			old, err = stage(data, info.Mode().Perm())
			if err != nil {
				return err
			}
		}
		staged, err := stage(f.data, f.mode)
		if err != nil {
			return err
		}
		changes = append(changes, change{dst, staged, old})
	}
	for i, c := range changes {
		if err := os.Rename(c.staged, c.dst); err != nil {
			for j := i - 1; j >= 0; j-- {
				prev := changes[j]
				if prev.old == "" {
					err = errors.Join(err, os.Remove(prev.dst))
				} else {
					err = errors.Join(err, os.Rename(prev.old, prev.dst))
				}
			}
			return err
		}
	}
	return nil
}

func writeRootConfig(path string, root *config.RootConfig) error {
	data, err := bindingConfig(path, root, nil)
	if err != nil {
		return err
	}
	return commitBindingFiles([]bindingFile{{path, data, 0600}})
}

func bindingConfig(src string, root *config.RootConfig, update *config.Config) ([]byte, error) {
	doc := make(map[string]any)
	data, err := os.ReadFile(src)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(data) != 0 {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, err
		}
		if doc == nil {
			doc = make(map[string]any)
		}
	}
	original := make(map[string]map[string]any)
	record := func(raw map[string]any) error {
		b, err := yaml.Marshal(raw)
		if err != nil {
			return err
		}
		var inst config.Config
		if err := yaml.Unmarshal(b, &inst); err != nil {
			return err
		}
		id, err := inst.AutoInstanceID()
		if err != nil {
			return err
		}
		if _, ok := original[id]; ok {
			return fmt.Errorf("duplicate existing binding %s", id)
		}
		original[id] = raw
		return nil
	}
	if raw, ok := doc["instances"]; ok {
		items, ok := raw.([]any)
		if raw != nil && !ok {
			return nil, errors.New("instances must be a sequence")
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("invalid instance")
			}
			if err := record(m); err != nil {
				return nil, err
			}
		}
	} else if root.Config.Panel.URL != "" || root.Config.IsStandalone() {
		old := make(map[string]any, len(doc))
		for k, v := range doc {
			old[k] = v
		}
		if err := record(old); err != nil {
			return nil, err
		}
		// These fields identify only the migrated binding. Shared settings stay at root.
		for _, k := range []string{"id", "panel", "machine", "nodes", "standalone", "health_port"} {
			delete(doc, k)
		}
	}
	items := make([]map[string]any, 0)
	for _, inst := range normalizeRootInstances(root) {
		id, err := inst.AutoInstanceID()
		if err != nil {
			return nil, err
		}
		m := original[id]
		if m == nil || (update != nil && update.InstanceID == id) {
			fresh := m == nil
			if fresh {
				m = make(map[string]any)
				m["log"] = map[string]any{"level": "info", "output": "stdout"}
			}
			section := func(name string) map[string]any {
				v, _ := m[name].(map[string]any)
				if v == nil {
					v = make(map[string]any)
					m[name] = v
				}
				return v
			}
			m["id"] = id
			p := section("panel")
			p["url"] = inst.Panel.URL
			if inst.IsMachineMode() {
				v := section("machine")
				v["machine_id"] = inst.Machine.MachineID
				v["token_env"] = inst.Machine.TokenEnv
				delete(v, "token")
			} else {
				p["node_id"] = inst.Panel.NodeID
				if inst.Panel.NodeType != "" {
					p["node_type"] = inst.Panel.NodeType
				}
				p["token_env"] = inst.Panel.TokenEnv
				delete(p, "token")
			}
			k := section("kernel")
			if fresh {
				k["config_dir"] = inst.Kernel.ConfigDir
				k["log_level"] = inst.Kernel.LogLevel
			}
			if inst.Kernel.Type != "" {
				k["type"] = inst.Kernel.Type
			}
			if inst.HealthPort != 0 {
				m["health_port"] = inst.HealthPort
			}
			if inst.Runtime.GoMemLimit != "" {
				section("runtime")["gomemlimit"] = inst.Runtime.GoMemLimit
			}
			if inst.Runtime.GoGCPercent != 0 {
				section("runtime")["gogc"] = inst.Runtime.GoGCPercent
			}
		}
		items = append(items, m)
	}
	doc["instances"] = items
	return yaml.Marshal(doc)
}

var envKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func editCredentials(src string, set map[string]string, remove map[string]bool) ([]byte, error) {
	data, err := os.ReadFile(src)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for k, v := range set {
		if !envKeyPattern.MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return nil, errors.New("invalid credential key or multiline value")
		}
	}
	seen := make(map[string]bool)
	var b strings.Builder
	for i, line := range strings.SplitAfter(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			b.WriteString(line)
			continue
		}
		k, _, ok := strings.Cut(trimmed, "=")
		if !ok || !envKeyPattern.MatchString(k) || seen[k] || strings.ContainsAny(strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), "\r\x00") {
			return nil, fmt.Errorf("invalid credentials at line %d", i+1)
		}
		seen[k] = true
		if v, ok := set[k]; ok {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		} else if !remove[k] {
			b.WriteString(line)
		}
	}
	for k, v := range set {
		if !seen[k] {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	return []byte(b.String()), nil
}

func saveBindingRemoval(cfg, creds, meta string, root *config.RootConfig, removed []config.Config) error {
	files := []bindingFile{}
	y, err := bindingConfig(cfg, root, nil)
	if err != nil {
		return err
	}
	files = append(files, bindingFile{cfg, y, 0600})
	keys := map[string]bool{}
	mark := func(inst config.Config, remove bool) {
		keys[inst.Panel.TokenEnv] = remove
		if inst.Machine != nil {
			keys[inst.Machine.TokenEnv] = remove
		}
	}
	for _, inst := range removed {
		mark(inst, true)
	}
	for _, inst := range root.Instances {
		mark(inst, false)
	}
	markShared := root.Config
	// The legacy root can still contain the binding just removed. Only protect
	// inherited credentials while some instance actually consumes them.
	if len(root.Instances) > 0 {
		mark(markShared, false)
	}
	if _, err := os.Stat(creds); err == nil {
		data, err := editCredentials(creds, nil, keys)
		if err != nil {
			return err
		}
		files = append(files, bindingFile{creds, data, 0600})
	} else if !os.IsNotExist(err) {
		return err
	}
	version := ""
	prev, err := loadInstallMeta(meta)
	if err == nil {
		version = prev.Version
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := installMetaBytes(root, version, "")
	if err != nil {
		return err
	}
	return commitBindingFiles(append(files, bindingFile{meta, data, 0644}))
}
