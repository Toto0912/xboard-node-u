package download

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadRedirectPolicy(t *testing.T) {
	var plainCalls atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainCalls.Add(1) }))
	defer plain.Close()
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer cdn.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cdn":
			http.Redirect(w, r, cdn.URL, 302)
		case "/loop":
			http.Redirect(w, r, "/loop", 302)
		default:
			http.Redirect(w, r, plain.URL, 302)
		}
	}))
	defer origin.Close()
	client := Client(time.Second*5, origin.Client().Transport)
	for _, url := range []string{cdn.URL, origin.URL + "/cdn"} {
		res, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	for _, url := range []string{plain.URL, origin.URL, origin.URL + "/loop"} {
		if res, err := client.Get(url); err == nil {
			res.Body.Close()
			t.Fatalf("accepted %s", url)
		}
	}
	if plainCalls.Load() != 0 {
		t.Fatal("plaintext destination was contacted")
	}
}
