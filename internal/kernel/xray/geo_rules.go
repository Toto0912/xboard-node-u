package xray

import "strings"

// Inspect the final Xray rule so panel, YAML and native JSON rules use the
// same resource discovery. Other fields such as outboundTag are literal tags.
func nativeGeoNeeds(rule M) (ip, site bool) {
	for key, input := range rule {
		var prefix string
		switch strings.ToLower(key) {
		case "ip", "source", "sourceip", "localip":
			prefix = "geoip:"
		case "domain", "domains":
			prefix = "geosite:"
		default:
			continue
		}
		match := func(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), prefix) }
		found := false
		switch v := input.(type) {
		case string:
			for part := range strings.SplitSeq(v, ",") {
				if match(part) {
					found = true
					break
				}
			}
		case []string:
			for _, part := range v {
				if match(part) {
					found = true
					break
				}
			}
		case []any:
			for _, part := range v {
				if s, ok := part.(string); ok && match(s) {
					found = true
					break
				}
			}
		}
		if found {
			if prefix == "geoip:" {
				ip = true
			} else {
				site = true
			}
		}
	}
	return ip, site
}
