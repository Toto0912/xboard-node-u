package main

import (
	"bytes"
	"github.com/cedar2025/xboard-node/internal/config"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"testing"
)

func TestBindingEditorsPreserveCustomFieldsAndLastRemoval(t *testing.T) {
	dir := t.TempDir()
	cfg, cred, meta := filepath.Join(dir, "config.yml"), filepath.Join(dir, "cred.env"), filepath.Join(dir, "meta.json")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg, "panel:\n  url: https://a.invalid\n  node_id: 3\n  token_env: SHARED\nkernel:\n  type: xray\n  custom_config: /one\n  geo_data_dir: /geo\n  future_setting: preserved\nfuture_section:\n  nested: [[true, 2]]\n")
	write(cred, "# retained\nSHARED=first\n")
	args := []string{"--config", cfg, "--output", cfg, "--credentials-in", cred, "--credentials-out", cred, "--meta", meta, "--mode", "node", "--panel-url", "https://b.invalid", "--node-id", "8", "--kernel", "singbox", "--token", "new-value"}
	if err := runConfigInit(args); err != nil {
		t.Fatal(err)
	}
	root, err := loadWritableRootConfig(cfg)
	if err != nil || len(root.Instances) != 2 {
		t.Fatalf("after add: %v", err)
	}
	b := root.Instances[1]
	key := b.Panel.TokenEnv
	if data, _ := os.ReadFile(cfg); bytes.Contains(data, []byte("new-value")) {
		t.Fatal("expanded token in config")
	}
	credData, _ := os.ReadFile(cred)
	if !bytes.Contains(credData, []byte("# retained\nSHARED=first\n")) {
		t.Fatal("other credential changed")
	}
	// Rebinding should update only the selected instance's token and preserve hand-edited fields.
	data, _ := os.ReadFile(cfg)
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	nodes := doc["instances"].([]any)
	nodes[1].(map[string]any)["custom_future"] = map[string]any{"values": []any{"x", "y"}}
	data, _ = yaml.Marshal(doc)
	write(cfg, string(data))
	args[len(args)-1] = "updated"
	if err := runConfigInit(args); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(cfg)
	for _, s := range []string{"custom_future", "future_section", "future_setting", "/one", "/geo"} {
		if !bytes.Contains(data, []byte(s)) {
			t.Fatalf("lost %s", s)
		}
	}
	credData, _ = os.ReadFile(cred)
	if !bytes.Contains(credData, []byte(key+"=updated\n")) {
		t.Fatal("target token not updated")
	}
	root, err = loadWritableRootConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	root.Instances = root.Instances[:1]
	if err := saveBindingRemoval(cfg, cred, meta, root, []config.Config{b}); err != nil {
		t.Fatal(err)
	}
	credData, _ = os.ReadFile(cred)
	if string(credData) != "# retained\nSHARED=first\n" {
		t.Fatal("incorrect credentials after removal")
	}
	root, err = loadWritableRootConfig(cfg)
	if err != nil || len(root.Instances) != 1 {
		t.Fatal("wrong instance removed")
	}
	// Unbind a legacy single instance directly.
	write(cfg, "panel:\n  url: https://a.invalid\n  node_id: 3\n  token_env: SHARED\n")
	root, err = loadWritableRootConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	removed := normalizeRootInstances(root)
	root.Instances = []config.Config{}
	if err := saveBindingRemoval(cfg, cred, meta, root, removed); err != nil {
		t.Fatal(err)
	}
	root, err = loadWritableRootConfig(cfg)
	if err != nil || len(normalizeRootInstances(root)) != 0 {
		t.Fatal("legacy instance resurrected")
	}
	md, err := loadInstallMeta(meta)
	if err != nil || md.InstanceCount != 0 {
		t.Fatal("metadata resurrected instance")
	}
}
func TestBindingFailurePreservesAllFiles(t *testing.T) {
	for _, mode := range []string{"malformed", "multiline", "metadata", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			d := t.TempDir()
			cfg, cred, meta := filepath.Join(d, "c"), filepath.Join(d, "e"), filepath.Join(d, "m")
			original := []byte("instances: []\n")
			if err := os.WriteFile(cfg, original, 0600); err != nil {
				t.Fatal(err)
			}
			raw := "# keep\nOLD=before\n"
			token := "example"
			src := cred
			switch mode {
			case "malformed":
				raw += "invalid-entry\n"
			case "multiline":
				token += "\nline"
			case "metadata":
				meta = d
			case "unreadable":
				src = d
			}
			if err := os.WriteFile(cred, []byte(raw), 0400); err != nil {
				t.Fatal(err)
			}
			err := runConfigInit([]string{"--config", cfg, "--credentials-in", src, "--credentials-out", cred, "--meta", meta, "--mode", "node", "--panel-url", "https://b.invalid", "--node-id", "8", "--token", token})
			if err == nil {
				t.Fatal("invalid operation succeeded")
			}
			cd, _ := os.ReadFile(cfg)
			ed, _ := os.ReadFile(cred)
			if !bytes.Equal(cd, original) || string(ed) != raw {
				t.Fatal("partial update")
			}
			if bytes.Contains([]byte(err.Error()), []byte(token)) {
				t.Fatal("secret in error")
			}
		})
	}
}

func TestUpgradeCopyAndVersion(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "original"), filepath.Join(d, "backup")
	data := bytes.Repeat([]byte("node"), 1<<17)
	if e := os.WriteFile(src, data, 0755); e != nil {
		t.Fatal(e)
	}
	if e := copyFile(src, dst); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(dst)
	if e != nil || !bytes.Equal(data, got) {
		t.Fatal("backup mismatch")
	}
	if e := copyFile(d, dst); e == nil {
		t.Fatal("expected copy error")
	}
	got, _ = os.ReadFile(dst)
	if !bytes.Equal(data, got) {
		t.Fatal("failed backup replaced good file")
	}
	if v := binaryVersion("xboard-node v2.4.1 (built 2026)\nsing-box v1\nXray core\n"); v != "v2.4.1" {
		t.Fatal(v)
	}
}
