package geodata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/download"
)

func TestGeoDataBoundedAtomicWrite(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/declared":
			w.Header().Set("Content-Length", "5")
		case "/broken":
			w.Header().Set("Content-Length", "4")
			fmt.Fprint(w, "x")
			return
		case "/chunked":
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "12345")
			return
		}
		fmt.Fprint(w, "1234")
	}))
	defer s.Close()
	old := httpClient
	httpClient = download.Client(time.Minute, s.Client().Transport)
	defer func() { httpClient = old }()
	dir := t.TempDir()
	path := filepath.Join(dir, "geo.dat")
	if err := downloadFile(path, s.URL, 4); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/declared", "/broken", "/chunked"} {
		if downloadFile(path, s.URL+suffix, 4) == nil {
			t.Fatal(suffix)
		}
		data, err := os.ReadFile(path)
		entries, dirErr := os.ReadDir(dir)
		if err != nil || dirErr != nil || string(data) != "1234" || len(entries) != 1 {
			t.Fatal(suffix, entries, err, dirErr)
		}
	}
}

func TestConcurrentGeoDownloadsPublishOneCompleteFile(t *testing.T) {
	d := t.TempDir()
	var calls atomic.Int64
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte("known fixture")) }))
	defer s.Close()
	old := httpClient
	httpClient = download.Client(time.Minute, s.Client().Transport)
	t.Cleanup(func() { httpClient = old })
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ensureFile(context.Background(), d, "geo.dat", s.URL); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := os.ReadFile(filepath.Join(d, "geo.dat"))
	entries, _ := os.ReadDir(d)
	if err != nil || string(b) != "known fixture" || calls.Load() != 1 || len(entries) != 1 {
		t.Fatalf("calls=%d entries=%d err=%v", calls.Load(), len(entries), err)
	}
}
