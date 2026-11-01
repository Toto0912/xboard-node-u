package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// XrayFallback is only exposed under standalone.node, never in panel DTOs.
type XrayFallback struct {
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	ALPN string `yaml:"alpn,omitempty" json:"alpn,omitempty"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	Dest any    `yaml:"dest" json:"dest"`
	Xver int    `yaml:"xver,omitempty" json:"xver,omitempty"`
}

// FallbackALPN supplies the TLS negotiation list for the standalone fallback
// listener. Keep this separate from the existing panel TLS translation.
func (n StandaloneNodeConfig) FallbackALPN() ([]string, error) {
	raw, ok := n.TLSSettings["alpn"]
	if !ok {
		return []string{"http/1.1"}, nil
	}
	var values []string
	switch list := raw.(type) {
	case []string:
		values = append(values, list...)
	case []any:
		for _, item := range list {
			value, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("standalone.node.tls_settings.alpn must be a non-empty list of strings")
			}
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("standalone.node.tls_settings.alpn must be a non-empty list of strings")
	}
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" {
			return nil, fmt.Errorf("standalone.node.tls_settings.alpn contains an empty or padded value")
		}
	}
	return values, nil
}

func (n StandaloneNodeConfig) ValidateFallbacks(kernelType string) error {
	if len(n.Fallbacks) == 0 {
		return nil
	}
	if kernelType != "xray" {
		return fmt.Errorf("standalone.node.fallbacks requires kernel.type: xray")
	}
	if n.Protocol != "vless" && n.Protocol != "trojan" {
		return fmt.Errorf("standalone.node.fallbacks only supports vless or trojan")
	}
	if n.Network != "" && n.Network != "tcp" && n.Network != "raw" {
		return fmt.Errorf("standalone.node.fallbacks requires TCP transport (network: tcp or raw)")
	}
	if n.TLS != 1 {
		return fmt.Errorf("standalone.node.fallbacks requires TLS (tls: 1); Reality uses tls_settings.dest instead")
	}
	if n.Protocol == "vless" && n.Decryption != "" && n.Decryption != "none" {
		return fmt.Errorf("standalone.node.fallbacks cannot be combined with VLESS decryption")
	}
	alpn, err := n.FallbackALPN()
	if err != nil {
		return err
	}
	for i, fallback := range n.Fallbacks {
		prefix := fmt.Sprintf("standalone.node.fallbacks[%d]", i)
		if fallback.Xver < 0 || fallback.Xver > 2 {
			return fmt.Errorf("%s.xver must be 0, 1 or 2", prefix)
		}
		if fallback.Path != "" && !strings.HasPrefix(fallback.Path, "/") {
			return fmt.Errorf("%s.path must be empty or start with /", prefix)
		}
		if fallback.ALPN == "h2" && fallback.Path != "" {
			return fmt.Errorf("%s.path cannot match HTTP/2 traffic", prefix)
		}
		if fallback.ALPN != "" {
			found := false
			for _, value := range alpn {
				found = found || value == fallback.ALPN
			}
			if !found {
				return fmt.Errorf("%s.alpn must be included in standalone.node.tls_settings.alpn", prefix)
			}
		}
		if err := validateFallbackDest(fallback.Dest); err != nil {
			return fmt.Errorf("%s.dest: %w", prefix, err)
		}
	}
	return nil
}

func validateFallbackDest(dest any) error {
	var value string
	switch v := dest.(type) {
	case int:
		value = strconv.Itoa(v)
	case uint64:
		value = strconv.FormatUint(v, 10)
	case string:
		value = v
	default:
		return fmt.Errorf("must be a port number, host:port or Unix socket path")
	}
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("must not be empty, padded or contain control characters")
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "@") {
		if strings.Trim(value, "/@") == "" {
			return fmt.Errorf("Unix socket path must include a name")
		}
		return nil
	}
	port := value
	if strings.Contains(value, ":") {
		host, p, err := net.SplitHostPort(value)
		if err != nil || strings.TrimSpace(host) == "" || strings.TrimSpace(host) != host {
			return fmt.Errorf("must be a valid host:port (IPv6 requires brackets)")
		}
		port = p
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("TCP port must be between 1 and 65535")
	}
	return nil
}
