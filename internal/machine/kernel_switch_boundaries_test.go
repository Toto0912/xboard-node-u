package machine

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/panel"
	"google.golang.org/protobuf/encoding/protowire"
)

type switchBoundaryNode struct {
	*Orchestrator
	ctx       context.Context
	addr      string
	tcp       panel.NodeConfig
	first     *nodeHandle
	bound     atomic.Bool
	userReply atomic.Value // JSON response from GET /user
	respond   atomic.Value // http.HandlerFunc for GET /config
}

func newSwitchBoundaryNode(t *testing.T, localOutbound bool, pullSeconds int) *switchBoundaryNode {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &switchBoundaryNode{
		addr: ln.Addr().String(),
		tcp: panel.NodeConfig{
			Protocol: "vless", Network: "tcp", ListenIP: "127.0.0.1", ServerPort: ln.Addr().(*net.TCPAddr).Port,
		},
	}
	ln.Close()
	f.bound.Store(true)
	f.userReply.Store(`{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`)
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(f.tcp)
	}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			if f.bound.Load() {
				fmt.Fprint(w, `{"nodes":[{"id":7,"type":"vless"}]}`)
			} else {
				fmt.Fprint(w, `{"nodes":[]}`)
			}
		case "/api/v2/server/config":
			f.respond.Load().(http.HandlerFunc)(w, r)
		case "/api/v2/server/user":
			fmt.Fprint(w, f.userReply.Load().(string))
		default:
			fmt.Fprint(w, "{}")
		}
	}))
	t.Cleanup(srv.Close)
	k := config.KernelConfig{Type: "singbox", ConfigDir: filepath.Join(t.TempDir(), "nodes"), LogLevel: "error"}
	if localOutbound {
		k.CustomOutbound = []map[string]any{{"type": "direct", "tag": "local-direct"}}
	}
	f.Orchestrator = New(&config.Config{
		Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "test"},
		Kernel: k, Node: config.NodeConfig{TrackInterval: 60, PullInterval: pullSeconds, DeviceReportInterval: 60},
		WS: config.WSConfig{DiscoveryInterval: 3600},
	})
	var cancel context.CancelFunc
	f.ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); f.stopChildren() })
	f.rediscover(f.ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp4", f.addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("initial sing-box listener did not open", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.first = f.nodes[7]
	f.waitReady(t, f.first)
	return f
}

func (f *switchBoundaryNode) waitReady(t *testing.T, h *nodeHandle) {
	t.Helper()
	switchTestWait(t, "child to commit its initial snapshot", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return h.ready
	})
}

func TestKernelSwitchHintDuringBootstrapWakesOnce(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	f.stopNode(7)
	var reads atomic.Int32
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reads.Add(1) == 2 { // Initial reads the snapshot after kernel selection.
			close(started)
			<-resume
		}
		_ = json.NewEncoder(w).Encode(f.tcp)
	}))
	f.rediscover(f.ctx)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap did not start")
	}
	for i := 0; i < 10; i++ {
		f.pushConfig("xhttp")
	}
	if len(f.discoveryWake) != 0 {
		t.Fatal("an unready child woke discovery before its snapshot was committed")
	}
	release()
	select {
	case <-f.discoveryWake:
	case <-time.After(5 * time.Second):
		t.Fatal("pending hints did not wake discovery on bootstrap completion")
	}
	f.rediscover(f.ctx)
	if reads.Load() != 3 || len(f.discoveryWake) != 0 {
		t.Fatalf("expected one confirmation after bootstrap, got %d total config reads", reads.Load())
	}
}

func (f *switchBoundaryNode) pushConfig(network string) {
	cfg := f.tcp
	cfg.Network = network
	f.onWSEvent(panel.WSEvent{Type: panel.WSEventSyncConfig, NodeID: 7, Config: &cfg})
}

func (f *switchBoundaryNode) requireOriginalListener(t *testing.T) {
	t.Helper()
	if f.nodes[7] != f.first {
		t.Fatal("healthy child replaced")
	}
	select {
	case <-f.first.done:
		t.Fatal("healthy child stopped")
	default:
	}
	c, err := net.DialTimeout("tcp4", f.addr, time.Second)
	if err != nil {
		t.Fatal("original listener closed", err)
	}
	c.Close()
}

func TestKernelSwitchRejectsUnconfirmedOrInvalidCandidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		change func(*panel.NodeConfig)
	}{
		{"unconditional-304", http.StatusNotModified, nil},
		{"temporary-503", http.StatusServiceUnavailable, nil},
		{"already-back-to-tcp", http.StatusOK, func(c *panel.NodeConfig) { c.Network = "tcp" }},
		{"invalid-xhttp-mode", http.StatusOK, func(c *panel.NodeConfig) {
			c.NetworkSettings = map[string]any{"mode": "unsupported-mode"}
		}},
		{"missing-port", http.StatusOK, func(c *panel.NodeConfig) { c.ServerPort = 0 }},
		{"unsupported-protocol", http.StatusOK, func(c *panel.NodeConfig) { c.Protocol = "tuic" }},
		{"tls-without-certificate", http.StatusOK, func(c *panel.NodeConfig) { c.TLS = 1 }},
		{"unreadable-certificate-pair", http.StatusOK, func(c *panel.NodeConfig) {
			c.TLS = 1
			c.CertConfig = &panel.CertConfig{CertMode: "file", CertFile: "missing.crt", KeyFile: "missing.key"}
		}},
		{"invalid-inline-certificate", http.StatusOK, func(c *panel.NodeConfig) {
			c.TLS = 1
			c.CertConfig = &panel.CertConfig{CertMode: "content", CertContent: "bad cert", KeyContent: "bad key"}
		}},
		{"singbox-local-outbound", http.StatusOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwitchBoundaryNode(t, tc.name == "singbox-local-outbound", 3600)
			var reads atomic.Int32
			f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.Header.Get("If-None-Match") != "" {
					t.Error("candidate confirmation used a cached ETag")
				}
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					cfg := f.tcp
					cfg.Network = "xhttp"
					if tc.change != nil {
						tc.change(&cfg)
					}
					_ = json.NewEncoder(w).Encode(cfg)
				}
			}))
			for i := 0; i < 10; i++ {
				f.pushConfig("xhttp")
			}
			if len(f.discoveryWake) != 1 {
				t.Fatal("XHTTP hints did not wake machine discovery")
			}
			<-f.discoveryWake
			f.rediscover(f.ctx)
			if n := reads.Load(); n != 1 {
				t.Fatalf("10 duplicate hints caused %d config reads", n)
			}
			f.requireOriginalListener(t)
			// More hints arriving after failure, or unrelated rediscoveries in
			// the same interval, must not repeatedly query the panel.
			for i := 0; i < 10; i++ {
				f.pushConfig("xhttp")
			}
			if len(f.discoveryWake) != 0 {
				t.Fatal("repeated hint bypassed retry interval")
			}
			f.rediscover(f.ctx)
			if n := reads.Load(); n != 1 {
				t.Fatalf("retry interval allowed %d reads", n)
			}
		})
	}
}

func TestKernelSwitchDiscardsConfirmationInvalidatedInFlight(t *testing.T) {
	for _, event := range []string{"sync.config-tcp", "sync.config-different-xhttp", panel.WSEventSyncNodes, panel.WSEventMachineRevoked} {
		t.Run(event, func(t *testing.T) {
			f := newSwitchBoundaryNode(t, false, 3600)
			started, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cfg := f.tcp
				cfg.Network = "xhttp"
				if reads.Add(1) == 1 {
					close(started)
					<-resume
				} else if event == "sync.config-different-xhttp" {
					cfg.NetworkSettings = map[string]any{"mode": "unsupported-mode"}
				}
				_ = json.NewEncoder(w).Encode(cfg)
			}))
			f.pushConfig("xhttp")
			go func() { defer close(finished); f.rediscover(f.ctx) }()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(resume) }) }
			t.Cleanup(func() { release(); <-finished })
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("authoritative confirmation did not begin")
			}
			switch event {
			case "sync.config-tcp":
				f.pushConfig("tcp")
			case "sync.config-different-xhttp":
				cfg := f.tcp
				cfg.Network = "xhttp"
				cfg.NetworkSettings = map[string]any{"mode": "unsupported-mode"}
				f.onWSEvent(panel.WSEvent{Type: panel.WSEventSyncConfig, NodeID: 7, Config: &cfg})
			default:
				f.bound.Store(false)
				f.onWSEvent(panel.WSEvent{Type: event})
			}
			release()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("confirmation did not finish")
			}
			if event == "sync.config-tcp" || event == "sync.config-different-xhttp" {
				f.requireOriginalListener(t)
				return
			}
			if h := f.nodes[7]; h != nil && h != f.first {
				t.Fatal("outdated XHTTP response created a new child after assignment changed")
			}
			f.rediscover(f.ctx)
			if h := f.nodes[7]; h != nil {
				t.Fatal("unbound node still has a service")
			}
		})
	}
}

func TestKernelSwitchChecksBelongToCurrentChildOnly(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	f.stopNode(7)
	f.rediscover(f.ctx)
	next := f.nodes[7]
	if next == nil || next == f.first {
		t.Fatal("node ID was not assigned a new child")
	}
	f.waitReady(t, next)
	// The previous child's REST request may return after its service has
	// exited. The new owner's mailbox is the generation discriminator.
	f.requestKernelCheck(7, f.first.mailbox)
	f.pushConfig("tcp")
	if len(f.discoveryWake) != 0 {
		t.Fatal("expired child or compatible config triggered machine discovery")
	}
	f.pushConfig("xhttp")
	if len(f.discoveryWake) != 1 {
		t.Fatal("current child did not wake discovery")
	}
}

func TestKernelSwitchBootstrapRechecksConfirmedState(t *testing.T) {
	for _, scenario := range []string{"changed-tls", "missing-users"} {
		t.Run(scenario, func(t *testing.T) {
			f := newSwitchBoundaryNode(t, false, 3600)
			var reads atomic.Int32
			f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cfg := f.tcp
				cfg.Network = "xhttp"
				if reads.Add(1) > 1 {
					if scenario == "changed-tls" {
						cfg.TLS = 1
					} else {
						f.userReply.Store(`{"users": null}`)
					}
				}
				_ = json.NewEncoder(w).Encode(cfg)
			}))
			f.pushConfig("xhttp")
			f.rediscover(f.ctx)
			replacement := f.nodes[7]
			if replacement == nil || replacement == f.first {
				t.Fatal("fixture did not reach the replacement bootstrap")
			}
			select {
			case <-replacement.done:
			case <-time.After(3 * time.Second):
				t.Fatal("replacement accepted a snapshot outside the confirmed state")
			}
		})
	}
}

func TestRejectedGeoHandoffPreservesAssetEnvironment(t *testing.T) {
	for _, initiallySet := range []bool{true, false} {
		t.Run(fmt.Sprintf("initially-set-%t", initiallySet), func(t *testing.T) {
			f := newSwitchBoundaryNode(t, false, 3600)
			const key = "XRAY_LOCATION_ASSET"
			original := t.TempDir()
			t.Setenv(key, original)
			if !initiallySet {
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			geoDir := t.TempDir()
			f.cfg.Kernel.GeoDataDir = geoDir
			if err := os.WriteFile(filepath.Join(geoDir, "geoip.dat"), []byte("invalid protobuf"), 0600); err != nil {
				t.Fatal(err)
			}
			f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				cfg := f.tcp
				cfg.Network = "xhttp"
				cfg.Routes = []panel.RouteRule{{Match: []string{fmt.Sprintf("geoip:reject%d", cfg.ServerPort)}, Action: "direct"}}
				_ = json.NewEncoder(w).Encode(cfg)
			}))
			f.pushConfig("xhttp")
			f.rediscover(f.ctx)
			f.requireOriginalListener(t)
			if value, ok := os.LookupEnv(key); ok != initiallySet || (ok && value != original) {
				t.Fatalf("rejected handoff changed %s: set=%t value=%q", key, ok, value)
			}
		})
	}
}

func TestKernelSwitchUsesGeoAssetsFromConfiguredDirectory(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	t.Setenv("XRAY_LOCATION_ASSET", t.TempDir())
	geoDir := t.TempDir()
	f.cfg.Kernel.GeoDataDir = geoDir
	// A GeoIPList with one country and one /32 CIDR, without a network download.
	code := fmt.Sprintf("QA%d", f.tcp.ServerPort)
	cidr := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte{192, 0, 2, 1})
	cidr = protowire.AppendVarint(protowire.AppendTag(cidr, 2, protowire.VarintType), 32)
	geoIP := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), code)
	geoIP = protowire.AppendBytes(protowire.AppendTag(geoIP, 2, protowire.BytesType), cidr)
	list := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), geoIP)
	if err := os.WriteFile(filepath.Join(geoDir, "geoip.dat"), list, 0600); err != nil {
		t.Fatal(err)
	}
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cfg := f.tcp
		cfg.Network = "xhttp"
		cfg.Routes = []panel.RouteRule{{Match: []string{"geoip:" + code}, Action: "direct"}}
		_ = json.NewEncoder(w).Encode(cfg)
	}))
	f.pushConfig("xhttp")
	f.rediscover(f.ctx)
	if f.nodes[7] == nil || f.nodes[7] == f.first {
		t.Fatal("valid geoip rule kept the node on sing-box")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp4", f.addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Xray did not listen with the configured geoip.dat", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKernelSwitchUsesLocalCertificateWhenPanelOmitsOverride(t *testing.T) {
	f := newSwitchBoundaryNode(t, false, 3600)
	server := httptest.NewTLSServer(http.NotFoundHandler())
	leaf := server.Certificate()
	key, err := x509.MarshalPKCS8PrivateKey(server.TLS.Certificates[0].PrivateKey)
	server.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "local.crt"), filepath.Join(dir, "local.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
		t.Fatal(err)
	}
	f.stopNode(7)
	// The local config supports file paths even when cert_mode is implicit.
	f.cfg.Cert = config.CertConfig{CertFile: certFile, KeyFile: keyFile}
	f.rediscover(f.ctx)
	f.first = f.nodes[7]
	f.waitReady(t, f.first)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp4", f.addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sing-box did not start with local TLS files", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.respond.Store(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cfg := f.tcp
		cfg.Network, cfg.TLS = "xhttp", 1
		_ = json.NewEncoder(w).Encode(cfg)
	}))
	f.pushConfig("xhttp")
	f.rediscover(f.ctx)
	if f.nodes[7] == nil || f.nodes[7] == f.first {
		t.Fatal("omitted panel certificate prevented handoff with valid local TLS")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	tlsCfg := &tls.Config{RootCAs: roots, ServerName: "127.0.0.1"}
	deadline = time.Now().Add(5 * time.Second)
	for {
		c, err := tls.DialWithDialer(&net.Dialer{Timeout: 200 * time.Millisecond}, "tcp4", f.addr, tlsCfg)
		if err == nil {
			got := c.ConnectionState().PeerCertificates[0].Raw
			c.Close()
			if !bytes.Equal(got, leaf.Raw) {
				t.Fatal("Xray served a different certificate")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("Xray TLS handshake did not present the local certificate", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
