package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestConcurrentStaticNodeFailuresExit(t *testing.T) {
	const configEnv = "XBOARD_TEST_FAILING_STATIC_NODES"
	if path := os.Getenv(configEnv); path != "" {
		root, err := config.LoadRoot(path)
		if err != nil {
			t.Fatal(err)
		}
		runWithReload(root, path)
		return
	}

	// Both children must enter their handshake before the first failure cancels startup.
	var arrivals atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/handshake" {
			http.NotFound(w, r)
			return
		}
		if arrivals.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
			http.Error(w, "panel unavailable", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	data := fmt.Sprintf("panel:\n  url: %q\n  token: test\nnodes:\n  - node_id: 1\n  - node_id: 2\n", server.URL)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConcurrentStaticNodeFailuresExit$")
	cmd.Env = append(os.Environ(), configEnv+"="+path)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 ||
		arrivals.Load() != 2 || strings.Count(string(out), "node service exited with error") != 2 {
		t.Fatalf("handshakes=%d, exit=%v, deadline=%v\n%s", arrivals.Load(), err, ctx.Err(), out)
	}
}
