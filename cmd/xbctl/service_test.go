package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestSelectServiceManager(t *testing.T) {
	for _, tc := range []struct {
		systemd, openrc bool
		want            serviceManager
		fail            bool
	}{
		{true, false, systemd, false}, {false, true, openRC, false},
		{true, true, systemd, false}, {false, false, "", true},
	} {
		got, err := selectServiceManager(tc.systemd, tc.openrc)
		if got != tc.want || (err != nil) != tc.fail {
			t.Fatalf("%+v: %q, %v", tc, got, err)
		}
	}
}

func TestServiceActionsUseSelectedManager(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart", "enable", "disable"} {
		for _, manager := range []serviceManager{systemd, openRC} {
			t.Run(string(manager)+"/"+action, func(t *testing.T) {
				var invoked []string
				err := dispatchService(manager, action, nil, func(name string, args ...string) error {
					invoked = append([]string{name}, args...)
					return nil
				})
				want := []string{"systemctl", action, serviceName}
				if manager == openRC {
					want = []string{"rc-service", openRCService, action}
					if action == "enable" {
						want = []string{"rc-update", "add", openRCService, "default"}
					}
					if action == "disable" {
						want = []string{"rc-update", "del", openRCService, "default"}
					}
				}
				if err != nil || !reflect.DeepEqual(invoked, want) {
					t.Fatalf("%v, %v; want %v", invoked, err, want)
				}
			})
		}
	}
	wantErr := errors.New("service failed")
	if err := dispatchService(openRC, "restart", nil, func(string, ...string) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatal("service failure was swallowed", err)
	}
}

func TestOpenRCLogs(t *testing.T) {
	for _, tc := range []struct{ input, prefix []string }{
		{nil, []string{"-n", "80", "-F"}},
		{[]string{"-n", "25", "--no-pager"}, []string{"-n", "25"}},
		{[]string{"--follow", "--lines", "0"}, []string{"-n", "0", "-F"}},
	} {
		got, err := openRCLogArgs(tc.input)
		want := append(tc.prefix, "/var/log/xboard-node/node.log", "/var/log/xboard-node/error.log")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%v: %v, %v", tc.input, got, err)
		}
	}
	for _, args := range [][]string{{"-n"}, {"-n", "-1"}, {"--lines", "bad"}, {"/etc/passwd"}} {
		if _, err := openRCLogArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestSupervisorRequiresLiveBinaryChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix symlinks; covered by Alpine integration")
	}
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	if err := os.WriteFile(pidfile, []byte("42\n"), 0600); err != nil {
		t.Fatal(err)
	}
	children := filepath.Join(dir, "42", "task", "42", "children")
	if err := os.MkdirAll(filepath.Dir(children), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(children, []byte("43 44"), 0600); err != nil {
		t.Fatal(err)
	}
	if supervisedNodeRunning(dir, pidfile, defaultBinaryPath) {
		t.Fatal("supervisor without node reported running")
	}
	if err := os.MkdirAll(filepath.Join(dir, "43"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(defaultBinaryPath, filepath.Join(dir, "43", "exe")); err != nil {
		t.Fatal(err)
	}
	if !supervisedNodeRunning(dir, pidfile, defaultBinaryPath) {
		t.Fatal("live node not found")
	}
}

func TestMetadataPreservesConfigAndInlineTokens(t *testing.T) {
	for _, cfg := range []string{
		"panel:\n  url: https://panel.invalid\n  node_id: 3\n  token: node-secret\nkernel:\n  type: xray\n",
		"panel:\n  url: https://panel.invalid\nmachine:\n  machine_id: 2\n  token: machine-secret\nkernel:\n  type: singbox\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yml")
		meta := filepath.Join(dir, "meta.json")
		if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
			t.Fatal(err)
		}
		if err := runConfigMetadata([]string{"--config", path, "--meta", meta, "--version", "v1.0.1"}); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if string(data) != cfg {
			t.Fatal("metadata command rewrote configuration")
		}
		m, err := loadInstallMeta(meta)
		if err != nil || m.Version != "v1.0.1" || m.InstanceCount != 1 {
			t.Fatalf("%+v: %v", m, err)
		}
		root, err := loadWritableRootConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		before := normalizeRootInstances(root)[0]
		if err := writeRootConfig(path, root); err != nil {
			t.Fatal(err)
		}
		root, err = loadWritableRootConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		after := normalizeRootInstances(root)[0]
		if before.Panel.Token != after.Panel.Token {
			t.Fatal("node token lost when writing bindings")
		}
		if before.Machine != nil && before.Machine.Token != after.Machine.Token {
			t.Fatal("machine token lost when writing bindings")
		}
	}
}

func TestStandaloneMetadataDeploymentAndBindingCoexistence(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "standalone.yml.example"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yml")
	credPath := filepath.Join(dir, "credentials.env")
	metaPath := filepath.Join(dir, "install-meta.json")
	if err := os.WriteFile(cfgPath, example, 0600); err != nil {
		t.Fatal(err)
	}
	port, err := deploymentPort(cfgPath, credPath, dir, cfgPath)
	if err != nil || port != 0 {
		t.Fatalf("deploymentPort = (%d, %v), want (0, nil)", port, err)
	}
	if err := runConfigMetadata([]string{"--config", cfgPath, "--meta", metaPath, "--version", "v2.4.3"}); err != nil {
		t.Fatal(err)
	}
	meta, err := loadInstallMeta(metaPath)
	if err != nil || meta.InstanceCount != 1 || len(meta.Instances) != 1 {
		t.Fatalf("meta = %+v, err = %v", meta, err)
	}
	inst := meta.Instances[0]
	if inst.Mode != "standalone" || inst.PanelURL != "" || inst.NodeID != nil || inst.MachineID != nil {
		t.Fatalf("unexpected standalone summary: %+v", inst)
	}
	if got := formatPanel(inst.Mode, inst.PanelURL); got != "-" {
		t.Fatalf("formatPanel = %q, want -", got)
	}
	if got := formatTargetForMode(inst.Mode, inst.NodeID, inst.MachineID); got != "local" {
		t.Fatalf("formatTargetForMode = %q, want local", got)
	}
	// Adding a panel binding later must preserve the standalone instance.
	if err := runConfigInit([]string{
		"--config", cfgPath, "--output", cfgPath,
		"--credentials-in", credPath, "--credentials-out", credPath,
		"--meta", metaPath, "--mode", "node",
		"--panel-url", "https://panel.invalid", "--node-id", "5", "--token", "secret-token",
	}); err != nil {
		t.Fatal(err)
	}
	meta, err = loadInstallMeta(metaPath)
	if err != nil || meta.InstanceCount != 2 {
		t.Fatalf("after bind: meta = %+v, err = %v", meta, err)
	}
	if meta.Instances[0].Mode != "standalone" || meta.Instances[1].Mode != "node" {
		t.Fatalf("unexpected modes after coexistence bind: %+v", meta.Instances)
	}
}
