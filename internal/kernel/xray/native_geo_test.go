package xray

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/kernel"
	"github.com/cedar2025/xboard-node/internal/model"
	geo "github.com/xtls/xray-core/common/geodata"
	"google.golang.org/protobuf/proto"
)

type geoTestTransport struct {
	local *url.URL
	http.RoundTripper
}

func (tr geoTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "github.com" {
		return nil, fmt.Errorf("unexpected test resource host")
	}
	mapped := r.Clone(r.Context())
	mapped.URL.Host = tr.local.Host
	mapped.URL.Scheme = tr.local.Scheme
	mapped.Host = tr.local.Host
	return tr.RoundTripper.RoundTrip(mapped)
}
func TestNativeRouteAssetDiscoveryAndCancelledSharedStart(t *testing.T) {
	t.Setenv("XRAY_LOCATION_ASSET", "")
	dir := t.TempDir()
	custom := filepath.Join(dir, "native.json")
	err := os.WriteFile(custom, []byte(`{"routing":{"rules":[{"type":"field","ip":["geoip:XBTEST"],"outboundTag":"block"},{"type":"field","domain":["geosite:XBTEST"],"outboundTag":"block"}]}}`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.KernelConfig{Type: "xray", LogLevel: "error", CustomConfig: custom, GeoDataDir: filepath.Join(dir, "assets")}
	spec := func() *model.NodeSpec {
		return &model.NodeSpec{Protocol: "vless", Network: "tcp", ListenIP: "127.0.0.1", ServerPort: reserveFreeTCPPort(t)}
	}
	users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-4111-8111-111111111111"}}
	if err := PreflightConfig(cfg, spec(), users, kernel.TLSCert{}); err != nil {
		t.Fatal("native cold preflight", err)
	}
	pack := func(m proto.Message) []byte {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ip := pack(&geo.GeoIPList{Entry: []*geo.GeoIP{{Code: "XBTEST", Cidr: []*geo.CIDR{{Ip: []byte{192, 0, 2, 0}, Prefix: 24}}}}})
	site := pack(&geo.GeoSiteList{Entry: []*geo.GeoSite{{Code: "XBTEST", Domain: []*geo.Domain{{Value: "test.invalid"}}}}})
	active := make(chan struct{})
	unblock := make(chan struct{})
	var release sync.Once
	free := func() { release.Do(func() { close(unblock) }) }
	var ipCalls, siteCalls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "geoip.dat":
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			if ipCalls.Add(1) == 1 {
				close(active)
			}
			<-unblock
			w.Write(ip)
		case "geosite.dat":
			siteCalls.Add(1)
			w.Write(site)
		default:
			t.Errorf("unexpected resource %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	defer free()
	local, _ := url.Parse(srv.URL)
	orig := http.DefaultTransport
	http.DefaultTransport = geoTestTransport{local, orig}
	defer func() { http.DefaultTransport = orig }()
	first, second := New(cfg), New(cfg)
	defer first.Stop()
	defer second.Stop()
	if err := first.Start(spec(), users, kernel.TLSCert{}); err == nil {
		t.Fatal("failed IP download must not install config")
	}
	validSite, err := os.ReadFile(filepath.Join(cfg.GeoDataDir, "geosite.dat"))
	if err != nil || !bytes.Equal(validSite, site) {
		t.Fatal("successful independent resource was not preserved", err)
	}
	fail.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first.SetResourceContext(ctx)
	a, b := spec(), spec()
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- first.Start(a, users, kernel.TLSCert{}) }()
	select {
	case <-active:
	case <-time.After(2 * time.Second):
		t.Fatal("native route did not trigger download")
	}
	go func() { doneB <- second.Start(b, users, kernel.TLSCert{}) }()
	cancel()
	select {
	case err := <-doneA:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("wrong cancelled result", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled node remained blocked by shared download")
	}
	free()
	if err := <-doneB; err != nil {
		t.Fatal("remaining node failed to load native route data", err)
	}
	for name, data := range map[string][]byte{"geoip.dat": ip, "geosite.dat": site} {
		actual, err := os.ReadFile(filepath.Join(cfg.GeoDataDir, name))
		if err != nil || !bytes.Equal(actual, data) {
			t.Fatal("wrong asset", name, err)
		}
	}
	if ipCalls.Load() != 1 || siteCalls.Load() != 1 || !second.IsRunning() || first.IsRunning() {
		t.Fatalf("ip=%d site=%d unexpected instance state", ipCalls.Load(), siteCalls.Load())
	}
}
