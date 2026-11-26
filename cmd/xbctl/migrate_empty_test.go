package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateEmptyV241(t *testing.T) {
	meta := `{"version":"v2.4.1","config_mode":"instances","latest_instance_id":"","instance_count":0,"instances":[],"updated_at":"2026-11-26T07:21:32Z"}`
	alpine := "log:\n    level: info\n    output: stdout\nkernel:\n    type: singbox\n    config_dir: \"\"\n    log_level: warn\n"
	custom := "node:\n    track_interval: 30\nws:\n    handshake_timeout: 15\nruntime:\n    gogc: 50\ncert:\n    cert_mode: file\n    dns_env:\n        CF_API_TOKEN: test\n"
	for _, tc := range []struct {
		name, input, meta, want string
	}{
		{"old-systemd", "{}\n", meta, "instances: []\n"},
		{"old-openrc", alpine, meta, alpine + "instances: []\n"},
		{"shared", custom, meta, custom + "instances: []\n"},
		{"empty-input", "", meta, ""},
		{"null", "null\n", meta, ""},
		{"binding", alpine + "panel: {}\n", meta, ""},
		{"already-explicit", "instances: []\n", meta, ""},
		{"unknown-field", "node: {node_id: 3}", meta, ""},
		{"wrong-type", "node: {track_interval: invalid}", meta, ""},
		{"coercible", "log: {level: 32}", meta, ""},
		{"null-section", "log: null", meta, ""},
		{"duplicate", alpine + "log: {}\n", meta, ""},
		{"second-document", alpine + "---\npanel: {}\n", meta, ""},
		{"missing-count", alpine, strings.Replace(meta, `"instance_count":0,`, "", 1), ""},
		{"nonzero-count", alpine, strings.Replace(meta, `"instance_count":0`, `"instance_count":1`, 1), ""},
		{"string-count", alpine, strings.Replace(meta, `"instance_count":0`, `"instance_count":"0"`, 1), ""},
		{"duplicate-count", alpine, strings.Replace(meta, `"instance_count":0`, `"instance_count":1,"instance_count":0`, 1), ""},
		{"invalid-version", alpine, strings.Replace(meta, "v2.4.1", "v2.4.0", 1), ""},
		{"null-instances", alpine, strings.Replace(meta, "[]", "null", 1), ""},
		{"bound-instances", alpine, strings.Replace(meta, "[]", "[{}]", 1), ""},
		{"last-binding", alpine, strings.Replace(meta, `"latest_instance_id":""`, `"latest_instance_id":"one"`, 1), ""},
		{"unexpected-meta-field", alpine, strings.Replace(meta, `"version"`, `"nodes":[],"version"`, 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			in, m, out := filepath.Join(dir, "config.yml"), filepath.Join(dir, "install-meta.json"), filepath.Join(dir, "new.yml")
			for p, s := range map[string]string{in: tc.input, m: tc.meta} {
				if err := os.WriteFile(p, []byte(s), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"migrate-empty-v241", "--config", in, "--meta", m, "--output", out}
			err := runConfig(args)
			got, readErr := os.ReadFile(out)
			if tc.want == "" {
				if err == nil || !os.IsNotExist(readErr) {
					t.Fatalf("unexpected output %q, migration=%v read=%v", got, err, readErr)
				}
			} else {
				if err != nil || readErr != nil || string(got) != tc.want {
					t.Fatalf("output %q, migration=%v read=%v", got, err, readErr)
				}
				args[len(args)-1] = in
				if runConfig(args) == nil {
					t.Fatal("overwrote existing input")
				}
			}
			for p, s := range map[string]string{in: tc.input, m: tc.meta} {
				if data, err := os.ReadFile(p); err != nil || string(data) != s {
					t.Fatalf("modified input %s: %v", p, err)
				}
			}
		})
	}
}
