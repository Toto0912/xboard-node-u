package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpgradeAlreadyInProgress(t *testing.T) {
	dir := t.TempDir()
	lock, err := lockInstallDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	restarted := false
	err = upgradeRelease("http://127.0.0.1:1", "v2", "amd64", dir, filepath.Join(dir, "install.sh"), nil, nil, func(bool) error { restarted = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "lock installation") || restarted {
		t.Fatal("concurrent upgrade did not stop before downloading", err)
	}
}

func TestUpgradeVerifiedRelease(t *testing.T) {
	for _, scenario := range []string{"latest", "no-saved-installer", "damaged-node", "damaged-cli", "damaged-installer", "missing-installer", "duplicate-installer", "duplicate", "missing", "wrong-tag", "bad-redirect", "restart-fails", "late-exit", "metadata-fails"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			trace := filepath.Join(dir, "executions")
			t.Setenv("XB_TEST_EXEC", trace)
			payload := func(name, version string) string {
				return "#!/bin/sh\necho " + name + " >> \"$XB_TEST_EXEC\"\necho " + name + " " + version + "\n"
			}
			for _, name := range []string{"xboard-node", "xbctl"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(payload(name, "v1")), 0755); err != nil {
					t.Fatal(err)
				}
			}
			node := payload("xboard-node", "v2")
			cli := payload("xbctl", "v2")
			if scenario == "wrong-tag" {
				cli = payload("xbctl", "v1")
			}
			a := fmt.Sprintf("%x  xboard-node-linux-amd64\n", sha256.Sum256([]byte(node)))
			b := fmt.Sprintf("%x *xbctl-linux-amd64\n", sha256.Sum256([]byte(cli)))
			installerPath := filepath.Join(dir, "install.sh")
			script := "#!/bin/bash\n# v2\n"
			c := fmt.Sprintf("%x  install.sh\n", sha256.Sum256([]byte(script)))
			if scenario != "no-saved-installer" {
				if err := os.WriteFile(installerPath, []byte("# v1"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/releases/latest":
					to := "/releases/tag/v2"
					if scenario == "bad-redirect" {
						to = "/other"
					}
					http.Redirect(w, r, to, http.StatusFound)
				case "/releases/tag/v2", "/other":
				case "/releases/download/v2/SHA256SUMS.txt":
					fmt.Fprint(w, a)
					if scenario != "missing" {
						fmt.Fprint(w, b)
					}
					if scenario == "duplicate" {
						fmt.Fprint(w, b)
					}
					if scenario != "missing-installer" && scenario != "no-saved-installer" {
						fmt.Fprint(w, c)
					}
					if scenario == "duplicate-installer" {
						fmt.Fprint(w, c)
					}
				case "/releases/download/v2/install.sh":
					fmt.Fprint(w, script)
					if scenario == "damaged-installer" {
						fmt.Fprint(w, "#bad")
					}
				case "/releases/download/v2/xboard-node-linux-amd64":
					fmt.Fprint(w, node)
					if scenario == "damaged-node" {
						fmt.Fprint(w, "#bad")
					}
				case "/releases/download/v2/xbctl-linux-amd64":
					fmt.Fprint(w, cli)
					if scenario == "damaged-cli" {
						fmt.Fprint(w, "#bad")
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			var starts []string
			meta := filepath.Join(dir, "install-meta")
			if err := os.WriteFile(meta, []byte("v1"), 0644); err != nil {
				t.Fatal(err)
			}
			record := func(tag string, installer []byte) error {
				if other, err := lockInstallDir(dir); err == nil {
					other.Close()
					t.Fatal("upgrade lock released before recording metadata")
				}
				if scenario == "metadata-fails" {
					return fmt.Errorf("fixture metadata write failure")
				}
				files := []bindingFile{{path: meta, data: []byte(tag), mode: 0644}}
				if installer != nil {
					files = append(files, bindingFile{path: installerPath, data: installer, mode: 0755})
				}
				return commitBindingFiles(files)
			}
			err := upgradeRelease(server.URL+"/releases", "latest", "amd64", dir, installerPath, nil, record, func(bool) error {
				versions := []string{}
				for _, name := range []string{"xboard-node", "xbctl"} {
					out, e := exec.Command(filepath.Join(dir, name), "version").Output()
					if e != nil {
						return e
					}
					versions = append(versions, strings.TrimSpace(string(out)))
				}
				starts = append(starts, strings.Join(versions, ","))
				if scenario == "late-exit" {
					// An actual child exits after restart returned; the restored child remains alive.
					lifetime := "5"
					if len(starts) == 1 {
						lifetime = "0.05"
					}
					child := exec.Command("sleep", lifetime)
					if err := child.Start(); err != nil {
						return err
					}
					done := make(chan struct{})
					go func() { child.Wait(); close(done) }()
					defer func() { child.Process.Kill(); <-done }()
					return observeSystemdStart(180*time.Millisecond, 15*time.Millisecond, func() (string, error) {
						select {
						case <-done:
							return "", fmt.Errorf("MainPID exited")
						default:
							return fmt.Sprint(child.Process.Pid), nil
						}
					})
				}
				if scenario == "restart-fails" && len(starts) == 1 {
					return fmt.Errorf("fixture restart failure")
				}
				return nil
			})
			ok := scenario == "latest" || scenario == "no-saved-installer"
			wantMeta := "v1"
			if ok {
				wantMeta = "v2"
			}
			stored, readErr := os.ReadFile(meta)
			if readErr != nil || string(stored) != wantMeta {
				t.Fatalf("metadata=%q err=%v", stored, readErr)
			}
			saved, readErr := os.ReadFile(installerPath)
			if scenario == "no-saved-installer" {
				if !os.IsNotExist(readErr) {
					t.Fatalf("unexpected saved installer: %v", readErr)
				}
			} else {
				want := "# v1"
				if ok {
					want = script
				}
				if readErr != nil || string(saved) != want {
					t.Fatalf("saved installer=%q err=%v", saved, readErr)
				}
			}
			if (err == nil) != ok {
				t.Fatal(err)
			}
			if ok && strings.Join(starts, ";") != "xboard-node v2,xbctl v2" {
				t.Fatal(starts)
			}
			if (scenario == "restart-fails" || scenario == "late-exit" || scenario == "metadata-fails") && strings.Join(starts, ";") != "xboard-node v2,xbctl v2;xboard-node v1,xbctl v1" {
				t.Fatal(starts)
			}
			if !ok && scenario != "late-exit" && scenario != "restart-fails" && scenario != "metadata-fails" && len(starts) != 0 {
				t.Fatal(starts)
			}
			if !ok && scenario != "late-exit" && scenario != "restart-fails" && scenario != "metadata-fails" && scenario != "wrong-tag" {
				if _, err := os.Stat(trace); !os.IsNotExist(err) {
					t.Fatal("candidate executed before paired integrity validation")
				}
			}
			for _, name := range []string{"xboard-node", "xbctl"} {
				ver := "v1"
				if ok {
					ver = "v2"
				}
				data, e := os.ReadFile(filepath.Join(dir, name))
				if e != nil || string(data) != payload(name, ver) {
					t.Fatal("installed pair", name, e)
				}
			}
		})
	}
}

func TestRestoreStoppedService(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "systemctl")
	script := `#!/bin/sh
echo "$1" >> "$QA_CALLS"
case "$1" in
    stop) ;;
    show) cat "$QA_STATE" ;;
    reset-failed) [ "$QA_FAIL_RESET" = 0 ] || exit 1; echo inactive > "$QA_STATE" ;;
    *) exit 1 ;;
esac
`
	if err := os.WriteFile(tool, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	for _, test := range []struct {
		name, previous, current, failReset, calls string
		ok                                        bool
	}{
		{"candidate-failed", "inactive", "failed", "0", "stop show reset-failed show", true},
		{"already-stopped", "inactive", "inactive", "0", "stop show", true},
		{"previously-failed", "failed", "failed", "0", "stop show", true},
		{"reset-failed", "inactive", "failed", "1", "stop show reset-failed", false},
		{"original-failure-cleared", "failed", "inactive", "0", "stop show", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			calls := filepath.Join(dir, "calls")
			t.Setenv("QA_STATE", state)
			t.Setenv("QA_CALLS", calls)
			t.Setenv("QA_FAIL_RESET", test.failReset)
			if err := os.WriteFile(state, []byte(test.current), 0600); err != nil {
				t.Fatal(err)
			}
			err := restoreStoppedService(test.previous)
			if (err == nil) != test.ok {
				t.Fatalf("unexpected rollback result: %v", err)
			}
			data, err := os.ReadFile(calls)
			if err != nil || strings.Join(strings.Fields(string(data)), " ") != test.calls {
				t.Fatalf("systemctl calls %q: %v", data, err)
			}
		})
	}
}

func TestObservedSystemdState(t *testing.T) {
	dir := t.TempDir()
	// Exercise the production parser with the same properties systemctl show emits.
	tool := filepath.Join(dir, "systemctl")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' \"$QA_UNIT_PROPERTIES\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("QA_UNIT_PROPERTIES", "MainPID=21\nActiveState=active\nNRestarts=2")
	if got, err := serviceStartIdentity(); got != "21:2" || err != nil {
		t.Fatal(got, err)
	}
	t.Setenv("QA_UNIT_PROPERTIES", "ActiveState=activating\nMainPID=0\nNRestarts=3")
	if _, err := serviceStartIdentity(); err == nil {
		t.Fatal("restart loop accepted")
	}
	i := 0
	if err := observeSystemdStart(30*time.Millisecond, time.Millisecond, func() (string, error) { i++; return fmt.Sprint(i), nil }); err == nil {
		t.Fatal("changed process accepted")
	}
}
