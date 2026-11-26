package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStagedDeploymentResolvesPathsFromServiceDirectory(t *testing.T) {
	work := t.TempDir()
	staged := filepath.Join(t.TempDir(), "candidate.yml")
	installed := filepath.Join(t.TempDir(), "config.yml")
	creds := filepath.Join(work, "credentials.env")
	if err := os.WriteFile(filepath.Join(work, "routes.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	body := "panel: {url: 'https://panel.example', node_id: 1, token: fixture}\nkernel: {type: xray, custom_config: routes.json}\nhealth_port: 9991\n"
	if err := os.WriteFile(staged, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := deploymentPort(staged, creds, work, installed)
	if err != nil || got != 9991 {
		t.Fatal(got, err)
	}
	if _, err := deploymentPort(staged, creds, filepath.Dir(staged), installed); err == nil {
		t.Fatal("missing custom config passed preflight")
	}
}

func TestDeploymentHealthUsesRuntimeLayoutAndCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	credentials := filepath.Join(dir, "credentials.env")
	t.Setenv("R6_NODE_TOKEN", "")
	if err := os.WriteFile(credentials, []byte("R6_NODE_TOKEN=fake$literal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := "  - panel: {url: 'https://panel.example', node_id: 1, token_env: R6_NODE_TOKEN}\n"
	second := "  - panel: {url: 'https://panel.example', node_id: 2, token_env: R6_NODE_TOKEN}\n    health_port: 3312\n"
	for _, c := range []struct {
		name, config string
		want         int
		bad          bool
	}{
		{"empty", "instances: []\n", -1, false},
		{"disabled", "instances:\n" + first + "    health_port: 0\n" + second, 0, false},
		{"missing-port", "instances:\n" + first + second, 0, false},
		{"custom", "instances:\n" + first + "    health_port: 3311\n" + second, 3311, false},
		{"bad-port", "instances:\n" + first + "    health_port: 65536\n", 0, true},
		{"invalid", "instances: [\n", 0, true},
		{"missing-credential", "instances:\n  - panel: {url: 'https://example.test', node_id: 1, token_env: NON_EXISTENT_R6_TOKEN}\n", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(c.config), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := deploymentPort(path, credentials, dir, path)
			if got != c.want || (err != nil) != c.bad {
				t.Fatalf("got (%d,%v), want (%d,bad=%v)", got, err, c.want, c.bad)
			}
			if os.Getenv("R6_NODE_TOKEN") != "" {
				t.Fatal("inspection left credentials in caller environment")
			}
		})
	}
}
