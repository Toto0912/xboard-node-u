package config

import (
	"net"
	"strconv"
	"strings"
)

// NormalizeFallbackTarget is the panel's optional host:port field. Unlike the
// standalone native fallbacks list, it does not accept bare ports or sockets.
func NormalizeFallbackTarget(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 320 {
		return ""
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" || len(port) == 0 || len(port) > 5 {
		return ""
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return ""
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return ""
	}
	ip := net.ParseIP(host)
	if strings.HasPrefix(value, "[") {
		if ip == nil || !strings.Contains(host, ":") {
			return ""
		}
	} else if ip == nil {
		name := strings.TrimSuffix(host, ".")
		if len(host) > 253 || name == "" || strings.Trim(host, "0123456789.") == "" {
			return ""
		}
		for _, label := range strings.Split(name, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return ""
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return ""
				}
			}
		}
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(n))
}
