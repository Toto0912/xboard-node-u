package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cedar2025/xboard-node/internal/config"
)

func TestReportIncludesExplicitEmptyDeviceSnapshot(t *testing.T) {
	received := make(chan map[string]json.RawMessage, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		received <- body
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()
	client := NewClient(config.PanelConfig{URL: server.URL, NodeID: 1, Token: "local-test"})
	if err := client.Report(nil, map[int][]string{}, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err != nil {
		t.Fatal(err)
	}
	body := <-received
	if string(body["alive"]) != "{}" {
		t.Fatalf("missing explicit empty snapshot: %s", body["alive"])
	}
}
