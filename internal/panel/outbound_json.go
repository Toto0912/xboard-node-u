package panel

import (
	"encoding/json"
	"fmt"
)

// Keep native fields through REST, machine discovery and WebSocket snapshots.
// A fixed legacy DTO alone silently discarded type, detour and streamSettings.
func (o *OutboundConfig) UnmarshalJSON(data []byte) error {
	type legacy OutboundConfig
	var value legacy
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var extra map[string]any
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	if extra == nil {
		return fmt.Errorf("custom outbound must be an object")
	}
	if _, native := extra["type"]; native {
		if value.Type == "" {
			return fmt.Errorf("custom outbound type must not be empty")
		}
		for _, key := range []string{"protocol", "settings", "proxy_tag"} {
			if _, mixed := extra[key]; mixed {
				return fmt.Errorf("custom outbound: do not mix type with %s", key)
			}
		}
	}
	for _, key := range []string{"tag", "protocol", "settings", "proxy_tag", "type"} {
		delete(extra, key)
	}
	*o = OutboundConfig(value)
	if len(extra) > 0 {
		o.Extra = extra
	}
	return nil
}

func (o OutboundConfig) MarshalJSON() ([]byte, error) {
	value := make(map[string]any, len(o.Extra)+5)
	for key, item := range o.Extra {
		value[key] = item
	}
	value["tag"] = o.Tag
	if o.Type != "" {
		value["type"] = o.Type
	} else {
		value["protocol"] = o.Protocol
	}
	if o.Settings != nil {
		value["settings"] = o.Settings
	}
	if o.ProxyTag != "" {
		value["proxy_tag"] = o.ProxyTag
	}
	return json.Marshal(value)
}
