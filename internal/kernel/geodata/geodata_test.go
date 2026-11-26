package geodata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentGeoDownloadsPublishOneCompleteFile(t *testing.T) {
	d := t.TempDir()
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte("known fixture")) }))
	defer s.Close()
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
