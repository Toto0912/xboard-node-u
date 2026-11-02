package model

import "github.com/cedar2025/xboard-node/internal/config"

type NodeSpec struct {
	Protocol        string
	ListenIP        string
	ServerPort      int
	Network         string
	NetworkSettings map[string]any
	Routes          []RouteRule

	KernelType       string
	KernelLogLevel   string
	CustomOutbounds  []OutboundConfig
	CustomRoutes     []map[string]any
	CustomRouteRules []CustomRouteRule
	CertConfig       *config.CertConfig
	AutoTLS          bool
	Domain           string

	Cipher    string
	Plugin    string
	PluginOpt string
	ServerKey string

	TLS         int
	Flow        string
	Decryption  string
	TLSSettings map[string]any

	Host       string
	ServerName string

	Version      int
	UpMbps       int
	DownMbps     int
	Obfs         string
	ObfsPassword string

	CongestionControl string
	PaddingScheme     string
	Transport         string
	TrafficPattern    string

	Multiplex           *MultiplexConfig
	AcceptProxyProtocol bool
	// Only the local control plane populates this. Omit it from legacy hashes.
	Standalone   *StandaloneOptions `json:",omitempty"`
	XrayFallback string             `json:",omitempty"`
}

// PanelFallbackTarget applies the same optional target rules to panel and
// standalone nodes. The sing-box control plane drops the field altogether.
func (n *NodeSpec) PanelFallbackTarget() string {
	if n == nil || (n.Protocol != "vless" && n.Protocol != "trojan") ||
		(n.TLS != 1 && n.TLS != 2) || (n.Network != "" && n.Network != "tcp" && n.Network != "raw") {
		return ""
	}
	if n.Protocol == "vless" && n.TLS == 1 && n.Decryption != "" && n.Decryption != "none" {
		return ""
	}
	return config.NormalizeFallbackTarget(n.XrayFallback)
}

// Call after ValidateNodeSpec so both control planes normalize the same way.
func (n *NodeSpec) normalizeXrayFallback(kcfg config.KernelConfig) {
	if n == nil {
		return
	}
	kind := kcfg.Type
	if kind == "" {
		kind = n.KernelType
	}
	kind, _ = normalizeKernelType(kind)
	if kind != "xray" {
		// Changes to an ignored option must not change sing-box reload hashes.
		n.XrayFallback = ""
	} else {
		n.XrayFallback = n.PanelFallbackTarget()
	}
}

type StandaloneOptions struct {
	Fallbacks []config.XrayFallback
	ALPN      []string
}

type OutboundConfig struct {
	Tag      string
	Protocol string
	Settings map[string]any
	ProxyTag string
}

type RouteRule struct {
	ID          int
	Match       []string
	Action      string
	ActionValue string
}

type MultiplexConfig struct {
	Enabled        bool
	Protocol       string
	MaxConnections int
	MinStreams     int
	MaxStreams     int
	Padding        bool
	Brutal         *BrutalConfig
}

type BrutalConfig struct {
	Enabled  bool
	UpMbps   int
	DownMbps int
}

type UserSpec struct {
	ID          int
	UUID        string
	SpeedLimit  int
	DeviceLimit int
}

func (n *NodeSpec) GetProxyProtocol() bool {
	if n == nil {
		return false
	}
	if n.AcceptProxyProtocol {
		return true
	}
	if n.NetworkSettings != nil {
		if v, ok := n.NetworkSettings["acceptProxyProtocol"]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func cloneMapSlice(src []map[string]any) []map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(src))
	for _, item := range src {
		out = append(out, cloneAnyMap(item))
	}
	return out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}
