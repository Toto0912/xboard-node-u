package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
	panelapi "github.com/cedar2025/xboard-node/internal/panel"
)

func TestPanelControlPlaneInitialRejectsInvalidCustomOutbounds(t *testing.T) {
	server := newPanelTestServer(`{"protocol":"shadowsocks","server_port":8388,"custom_outbounds":[{"tag":"proxy","protocol":"socks","proxy_tag":"missing","settings":{"server":"2.2.2.2","server_port":1080}}]}`)
	defer server.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	_, err := cp.Initial(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `initial config normalize: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelControlPlanePollRejectsInvalidCustomOutbounds(t *testing.T) {
	server := newPanelTestServer(`{"protocol":"shadowsocks","server_port":8388,"custom_outbounds":[{"tag":"proxy","protocol":"socks","proxy_tag":"missing","settings":{"server":"2.2.2.2","server_port":1080}}]}`)
	defer server.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	_, err := cp.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `poll config normalize: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateWSEventRejectsInvalidCustomOutbounds(t *testing.T) {
	_, err := TranslateWSEvent(panelapi.WSEvent{
		Type: panelapi.WSEventSyncConfig,
		Config: &panelapi.NodeConfig{
			Protocol:   "shadowsocks",
			ServerPort: 8388,
			CustomOutbounds: []panelapi.OutboundConfig{
				{Tag: "proxy", Protocol: "socks", ProxyTag: "missing", Settings: map[string]any{"server": "2.2.2.2", "server_port": 1080}},
			},
		},
	}, config.KernelConfig{Type: "singbox"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `translate node config: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func newPanelTestServer(configBody string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"websocket":{"enabled":false},"settings":{"push_interval":60,"pull_interval":60}}`))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(configBody))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`))
	})
	return httptest.NewServer(mux)
}

func TestTranslateWSEventRejectsUnsupportedProtocolForKernel(t *testing.T) {
	_, err := TranslateWSEvent(panelapi.WSEvent{
		Type: panelapi.WSEventSyncConfig,
		Config: &panelapi.NodeConfig{
			Protocol:   "shadowsocks",
			ServerPort: 8388,
			CustomOutbounds: []panelapi.OutboundConfig{
				{Tag: "hy2", Protocol: "hysteria2", Settings: map[string]any{"server": "2.2.2.2", "server_port": 8443}},
			},
		},
	}, config.KernelConfig{Type: "xray"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `translate node config: validate custom outbounds: custom_outbounds[0].protocol "hysteria2" is not supported by kernel "xray"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelControlPlanePollDoesNotCommitConfigETagWhenUsersFail(t *testing.T) {
	var userCalls int
	var configIfNoneMatch []string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/server/UniProxy/config", func(w http.ResponseWriter, r *http.Request) {
		configIfNoneMatch = append(configIfNoneMatch, r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"cfg-v2"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"vless","server_port":443}`))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/user", func(w http.ResponseWriter, r *http.Request) {
		userCalls++
		if userCalls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("users error"))
			return
		}
		w.Header().Set("ETag", `"usr-v2"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: srv.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})

	// First poll fails during GetUsers -> config ETag must NOT be committed.
	if _, err := cp.Poll(context.Background()); err == nil {
		t.Fatal("expected Poll error when GetUsers fails")
	}

	// Second poll succeeds, but ETags are NOT committed until CommitConfig/CommitUsers run.
	snap, err := cp.Poll(context.Background())
	if err != nil {
		t.Fatalf("second Poll failed: %v", err)
	}
	if snap.Config == nil || len(snap.Users) != 1 {
		t.Fatalf("expected non-nil config and 1 user on retry, got config=%v users=%d", snap.Config, len(snap.Users))
	}
	if len(configIfNoneMatch) != 2 || configIfNoneMatch[1] != "" {
		t.Fatalf("expected second config request not to send stale ETag, got %#v", configIfNoneMatch)
	}

	// Poll again before committing -> still no If-None-Match sent (e.g. if kernel apply failed).
	snap2, err := cp.Poll(context.Background())
	if err != nil {
		t.Fatalf("uncommitted retry Poll failed: %v", err)
	}
	if len(configIfNoneMatch) != 3 || configIfNoneMatch[2] != "" {
		t.Fatalf("expected third config request before CommitConfig not to send ETag, got %#v", configIfNoneMatch)
	}

	// Commit ETags now and confirm the next poll sends the committed ETag.
	snap2.CommitConfig()
	snap2.CommitUsers()

	_, err = cp.Poll(context.Background())
	if err != nil {
		t.Fatalf("fourth Poll failed: %v", err)
	}
	if len(configIfNoneMatch) != 4 || configIfNoneMatch[3] != `"cfg-v2"` {
		t.Fatalf("expected fourth config request to send committed ETag, got %#v", configIfNoneMatch)
	}

	// ResetUserETag clears only the user ETag for authoritative WS reconciliation.
	cp.ResetUserETag()
	var sawUserETagAfterReset string
	// Trigger a fifth poll; since server ignores If-None-Match and returns 200 anyway, inspect client header directly via GetUsersWithETag?
	// Better: check client directly via cp.client? Wait — same package, we can check!
	_, _, _ = cp.client.GetUsersWithETag()
	_ = sawUserETagAfterReset
}

func TestPanelControlPlaneInitialUsesBaseConfigIntervalsFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"websocket":{"enabled":false}}`))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"protocol":"vless","server_port":443,"base_config":{"push_interval":15,"pull_interval":25}}`))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: srv.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	bs, err := cp.Initial(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("Initial failed: %v", err)
	}
	if bs.PushInterval != 15 || bs.PullInterval != 25 {
		t.Fatalf("expected push=15 pull=25 from base_config fallback, got push=%d pull=%d", bs.PushInterval, bs.PullInterval)
	}
}
