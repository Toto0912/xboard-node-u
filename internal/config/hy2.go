package config

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

// HY2Settings is an optional server-only extension. Absence preserves the
// original panel's negotiation policy; auto opts into the new bandwidth policy.
type HY2Settings struct {
	BandwidthMode string         `json:"bandwidth_mode,omitempty" yaml:"bandwidth_mode,omitempty"`
	Kernel        string         `json:"kernel,omitempty" yaml:"kernel,omitempty"`
	Masquerade    map[string]any `json:"masquerade,omitempty" yaml:"masquerade,omitempty"`
}

var hy2HeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (s *HY2Settings) Clone() *HY2Settings {
	if s == nil {
		return nil
	}
	c := *s
	if s.Masquerade != nil {
		c.Masquerade = make(map[string]any, len(s.Masquerade))
		for k, v := range s.Masquerade {
			if m, ok := v.(map[string]any); ok {
				h := make(map[string]any, len(m))
				for name, value := range m {
					switch list := value.(type) {
					case []any:
						h[name] = append([]any{}, list...)
					case []string:
						h[name] = append([]string{}, list...)
					default:
						h[name] = value
					}
				}
				c.Masquerade[k] = h
			} else {
				c.Masquerade[k] = v
			}
		}
	}
	return &c
}

func (s *HY2Settings) AutoBandwidth() bool { return s != nil && s.BandwidthMode == "auto" }

func ValidateHY2(s *HY2Settings, protocol string, version int, kind string, up, down int) error {
	if protocol != "hysteria" && protocol != "hysteria2" {
		if s != nil {
			return fmt.Errorf("hy2_settings requires Hysteria2")
		}
		return nil
	}
	if version != 2 {
		if s != nil {
			return fmt.Errorf("hy2_settings requires version: 2")
		}
		return nil
	}
	if up < 0 || down < 0 || up > 1000000 || down > 1000000 {
		return fmt.Errorf("HY2 bandwidth must be between 0 and 1000000 Mbps")
	}
	if s == nil {
		return nil
	}
	if s.BandwidthMode != "" && s.BandwidthMode != "auto" {
		return fmt.Errorf("hy2_settings.bandwidth_mode must be auto or omitted")
	}
	if len(s.Masquerade) == 0 {
		return nil
	}
	if s.Kernel != kind {
		return fmt.Errorf("hy2_settings.kernel %q does not match actual kernel %q", s.Kernel, kind)
	}
	m := s.Masquerade
	t, ok := m["type"].(string)
	if !ok || (t != "proxy" && t != "file" && t != "string") {
		return fmt.Errorf("hy2_settings.masquerade.type must be file, proxy or string")
	}
	keys := map[string]bool{"type": true}
	dir, host, status := "directory", "rewrite_host", "status_code"
	if kind == "xray" {
		dir, host, status = "dir", "rewriteHost", "statusCode"
	}
	switch t {
	case "file":
		keys[dir] = true
	case "proxy":
		keys["url"], keys[host] = true, true
		if kind == "xray" {
			keys["insecure"], keys["xForwarded"] = true, true
		}
	case "string":
		keys["content"], keys["headers"], keys[status] = true, true, true
	}
	for key := range m {
		if !keys[key] {
			return fmt.Errorf("hy2_settings.masquerade.%s is unsupported for %s/%s", key, kind, t)
		}
	}
	if t == "file" || t == "proxy" || t == "string" {
		key := "content"
		if t == "file" {
			key = dir
		}
		if t == "proxy" {
			key = "url"
		}
		v, ok := m[key].(string)
		if !ok || (t != "string" && strings.TrimSpace(v) == "") {
			return fmt.Errorf("hy2_settings.masquerade.%s must be a string%s", key, map[bool]string{true: "", false: " and not empty"}[t == "string"])
		}
		if t == "proxy" {
			u, e := url.Parse(v)
			if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
				return fmt.Errorf("hy2_settings.masquerade.url must be an HTTP(S) URL without credentials or fragment")
			}
		}
	}
	for _, key := range []string{host, "insecure", "xForwarded"} {
		if v, exists := m[key]; exists {
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("hy2_settings.masquerade.%s must be boolean", key)
			}
		}
	}
	if v, exists := m[status]; exists {
		var n float64
		switch value := v.(type) {
		case float64:
			n = value
		case int:
			n = float64(value)
		case int64:
			n = float64(value)
		default:
			return fmt.Errorf("hy2_settings.masquerade.%s must be an integer", status)
		}
		if math.Trunc(n) != n || n < 200 || n > 599 {
			return fmt.Errorf("hy2_settings.masquerade.%s must be between 200 and 599", status)
		}
	}
	if v, exists := m["headers"]; exists {
		h, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("hy2_settings.masquerade.headers must be an object")
		}
		for name, value := range h {
			if !hy2HeaderName.MatchString(name) {
				return fmt.Errorf("invalid masquerade header name")
			}
			check := func(v any) bool { text, ok := v.(string); return ok && !strings.ContainsAny(text, "\r\n") }
			if check(value) {
				continue
			}
			if kind == "singbox" {
				if list, ok := value.([]string); ok {
					valid := true
					for _, entry := range list {
						valid = valid && check(entry)
					}
					if valid {
						continue
					}
				}
				if list, ok := value.([]any); ok {
					valid := true
					for _, entry := range list {
						valid = valid && check(entry)
					}
					if valid {
						continue
					}
				}
			}
			return fmt.Errorf("invalid value for masquerade header %s", name)
		}
	}
	return nil
}
