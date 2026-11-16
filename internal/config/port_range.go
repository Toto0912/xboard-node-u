package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseHY2PortRange deliberately accepts one continuous, nonempty port range.
func ParseHY2PortRange(value string) (int, int, error) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("hy2_settings.port_range must be an ascending range, for example 60000-65000")
	}
	ports := [2]int{}
	for i, part := range parts {
		if part == "" || len(part) > 5 {
			return 0, 0, fmt.Errorf("invalid HY2 port range %q", value)
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, 0, fmt.Errorf("invalid HY2 port range %q", value)
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 65535 {
			return 0, 0, fmt.Errorf("HY2 range ports must be between 1 and 65535")
		}
		ports[i] = n
	}
	if ports[0] >= ports[1] {
		return 0, 0, fmt.Errorf("HY2 range end must be greater than its first port; omit port_range for a single port")
	}
	return ports[0], ports[1], nil
}

func ValidateHY2PortRange(settings *HY2Settings, listenPort int) error {
	if settings == nil || settings.PortRange == "" {
		return nil
	}
	start, _, err := ParseHY2PortRange(settings.PortRange)
	if err != nil {
		return err
	}
	if start != listenPort {
		return fmt.Errorf("hy2_settings.port_range must start with server_port (%d)", listenPort)
	}
	return nil
}
