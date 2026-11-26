package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestReloadRotatesOwnedFilesAndReceivesTerminationDuringDrain(t *testing.T) {
	const childEnv = "XB_TEST_RELOAD_FILE"
	if path := os.Getenv(childEnv); path != "" {
		root, e := config.LoadRoot(path)
		if e != nil {
			t.Fatal(e)
		}
		instances, e := root.NormalizeInstances()
		if e != nil {
			t.Fatal(e)
		}
		config.InitLogger(instances[0].Log)
		runWithReload(root, path)
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "node.yml")
	var hold atomic.Bool
	var sessions atomic.Int32
	entered, other, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once, otherOnce, free sync.Once
	unblock := func() { free.Do(func() { close(release) }) }
	defer unblock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/handshake":
			sessions.Add(1)
			fmt.Fprint(w, `{"websocket":{"enabled":false}}`)
		case "/api/v1/server/UniProxy/config", "/api/v2/server/config":
			fmt.Fprint(w, `{"protocol":"vless","listen_ip":"127.0.0.1","server_port":24007,"network":"tcp"}`)
		case "/api/v1/server/UniProxy/user", "/api/v2/server/user":
			fmt.Fprint(w, `{"users":[]}`)
		case "/api/v2/server/report":
			var body struct {
				NodeID int `json:"node_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if hold.Load() {
				if body.NodeID == 1 {
					once.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
					}
				} else if body.NodeID == 2 {
					otherOnce.Do(func() { close(other) })
				}
			}
			fmt.Fprint(w, "{}")
		default:
			fmt.Fprint(w, "{}")
		}
	}))
	defer srv.Close()
	reserve := func() net.Listener {
		t.Helper()
		l, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		return l
	}
	write := func(port int, log string) {
		t.Helper()
		data := fmt.Sprintf("panel:\n  url: %q\n  token: fixture\nnodes:\n  - node_id: 1\n  - node_id: 2\nkernel:\n  type: xray\n  config_dir: %q\nhealth_port: %d\nlog:\n  level: info\n  output: %q\n", srv.URL, dir, port, filepath.Join(dir, log))
		if e := os.WriteFile(path, []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
	}
	l := reserve()
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	write(port, "first.log")
	ctx, cancel := context.WithTimeout(t.Context(), 65*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReloadRotatesOwnedFilesAndReceivesTerminationDuringDrain$")
	cmd.Env = append(os.Environ(), childEnv+"="+path)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	defer func() {
		unblock()
		cancel()
		if t.Failed() {
			select {
			case <-wait:
			case <-time.After(2 * time.Second):
			}
			t.Log(out.String())
		}
	}()
	waitFor := func(fn func() bool, why string) {
		t.Helper()
		end := time.Now().Add(6 * time.Second)
		for !fn() {
			if time.Now().After(end) {
				t.Fatal(why)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	fdCount := func() (int, int) {
		dirFD := fmt.Sprintf("/proc/%d/fd", cmd.Process.Pid)
		list, _ := os.ReadDir(dirFD)
		n := 0
		total := len(list)
		for _, e := range list {
			file, _ := os.Readlink(filepath.Join(dirFD, e.Name()))
			if strings.HasPrefix(file, dir+"/") && strings.HasSuffix(file, ".log") {
				n++
			}
		}
		return n, total
	}
	waitFor(func() bool { return sessions.Load() >= 2 }, "children did not bootstrap")
	_, base := fdCount()
	busy := reserve()
	defer busy.Close()
	write(busy.Addr().(*net.TCPAddr).Port, "rejected.log")
	waitFor(func() bool {
		d, _ := os.ReadFile(filepath.Join(dir, "first.log"))
		return strings.Contains(string(d), "invalid reload health listener")
	}, "bound health port was not rejected")
	if sessions.Load() != 2 {
		t.Fatal("invalid reload restarted children")
	}
	var activeLog string
	for i := 0; i < 4; i++ {
		l = reserve()
		next := l.Addr().(*net.TCPAddr).Port
		l.Close()
		log := fmt.Sprintf("rotation-%d.log", i)
		write(next, log)
		want := int32(2 * (i + 2))
		waitFor(func() bool { return sessions.Load() >= want }, "replacement children did not bootstrap")
		waitFor(func() bool {
			n, _ := fdCount()
			d, _ := os.ReadFile(filepath.Join(dir, log))
			return n == 1 && strings.Contains(string(d), "reload complete")
		}, "wrong file ownership or log destination")
		n, total := fdCount()
		t.Logf("rotation %d: open log files %d, descriptors %d (initial %d)", i, n, total, base)
		if total > base+4 {
			t.Fatalf("descriptors grew beyond active connection allowance")
		}
		l, e := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if e != nil {
			t.Fatal("retired health port busy", e)
		}
		l.Close()
		client := http.Client{Timeout: time.Second}
		resp, e := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", next))
		if e != nil {
			t.Fatal(e)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
		port = next
		activeLog = filepath.Join(dir, log)
	}
	before := sessions.Load()
	hold.Store(true)
	drainStarted := time.Now()
	write(port, "after-termination.log")
	for _, finished := range []chan struct{}{entered, other} {
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Fatal("expected both independent final reports")
		}
	}
	t.Logf("normal child sent final report while second report remained pending after %s", time.Since(drainStarted))
	if e := cmd.Process.Signal(syscall.SIGTERM); e != nil {
		t.Fatal(e)
	}
	// An explicit slow-path acceptance run lets the real HTTP client reach
	// its 30-second deadline. Regular regressions release it immediately.
	exitBudget := 5 * time.Second
	if os.Getenv("XB_TEST_HOLD_REPORT_TIMEOUT") == "1" {
		exitBudget = 40 * time.Second
	} else {
		// Signal delivery is asynchronous. Keep the report in flight until
		// the child has received SIGTERM, as required by this scenario.
		waitFor(func() bool {
			d, _ := os.ReadFile(activeLog)
			return strings.Contains(string(d), "shutting down")
		}, "child did not receive SIGTERM during drain")
		unblock()
	}
	select {
	case err := <-wait:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(exitBudget):
		t.Fatal("process exceeded its final report budget")
	}
	if sessions.Load() != before {
		t.Fatal("new children started after termination")
	}
	t.Logf("four reloads, two children, one open log file, baseline descriptors=%d, drain to exit %s", base, time.Since(drainStarted))
}
