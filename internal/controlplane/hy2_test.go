package controlplane

import (
	"github.com/cedar2025/xboard-node/internal/config"
	"github.com/cedar2025/xboard-node/internal/model"
	"testing"
)

func TestHY2MailboxOwnsMasqueradeHeaders(t *testing.T) {
	spec := &model.NodeSpec{HY2Settings: &config.HY2Settings{PortRange: "60000-65000", Masquerade: map[string]any{"headers": map[string]any{"X-Test": []any{"one"}}}}}
	copied := cloneNodeSpec(spec)
	if copied.HY2Settings.PortRange != spec.HY2Settings.PortRange {
		t.Fatal("WS mailbox lost HY2 port range")
	}
	copied.HY2Settings.Masquerade["headers"].(map[string]any)["X-Test"].([]any)[0] = "changed"
	if spec.HY2Settings.Masquerade["headers"].(map[string]any)["X-Test"].([]any)[0] != "one" {
		t.Fatal("WS mailbox snapshot shares masquerade headers")
	}
}
