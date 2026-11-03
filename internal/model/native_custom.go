package model

import (
	"fmt"
	"strings"
)

func nativeBuiltin(protocol, kernel string) bool {
	if kernel == "xray" {
		return protocol == "direct" || protocol == "block" || protocol == "freedom" || protocol == "blackhole" || protocol == "dns" || protocol == "loopback"
	}
	return false
}

func exactOutboundTags(outbounds []OutboundConfig, additional []string) map[string]struct{} {
	tags := map[string]struct{}{"direct": {}, "block": {}}
	for _, outbound := range outbounds {
		tags[outbound.Tag] = struct{}{}
	}
	for _, tag := range additional {
		tags[tag] = struct{}{}
	}
	return tags
}

func checkOutboundReference(value any, path string, tags map[string]struct{}, self string) error {
	tag, ok := value.(string)
	if !ok || strings.TrimSpace(tag) == "" {
		return fmt.Errorf("%s must be a nonempty string", path)
	}
	if tag == self && self != "" {
		return fmt.Errorf("%s must not reference itself", path)
	}
	if _, exists := tags[tag]; !exists {
		return fmt.Errorf("%s references unknown outbound %q (tags are case-sensitive)", path, tag)
	}
	return nil
}

func checkOptionalOutboundReference(value any, path string, tags map[string]struct{}, self string) error {
	if tag, ok := value.(string); ok && tag == "" {
		return nil
	}
	return checkOutboundReference(value, path, tags, self)
}

func validateNativeOutboundReferences(index int, outbound OutboundConfig, tags map[string]struct{}) error {
	path := fmt.Sprintf("custom_outbounds[%d]", index)
	if outbound.Type != "" {
		for _, field := range []string{"detour", "default"} {
			if value, ok := outbound.Extra[field]; ok {
				if err := checkOptionalOutboundReference(value, path+"."+field, tags, outbound.Tag); err != nil {
					return err
				}
			}
		}
		if value, ok := outbound.Extra["outbounds"]; ok {
			list, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s.outbounds must be an array", path)
			}
			for i, tag := range list {
				if err := checkOutboundReference(tag, fmt.Sprintf("%s.outbounds[%d]", path, i), tags, outbound.Tag); err != nil {
					return err
				}
			}
		}
	} else {
		if proxy, ok := outbound.Extra["proxySettings"]; ok {
			if outbound.ProxyTag != "" {
				return fmt.Errorf("%s: do not combine proxy_tag and proxySettings", path)
			}
			object, ok := proxy.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.proxySettings must be an object", path)
			}
			if value, ok := object["tag"]; ok {
				if err := checkOptionalOutboundReference(value, path+".proxySettings.tag", tags, outbound.Tag); err != nil {
					return err
				}
			}
		}
		if stream, ok := outbound.Extra["streamSettings"].(map[string]any); ok {
			if socket, ok := stream["sockopt"].(map[string]any); ok {
				if value, ok := socket["dialerProxy"]; ok {
					if err := checkOptionalOutboundReference(value, path+".streamSettings.sockopt.dialerProxy", tags, outbound.Tag); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateOutboundCycles(outbounds []OutboundConfig) error {
	edges := map[string][]string{}
	for _, outbound := range outbounds {
		if outbound.ProxyTag != "" {
			edges[outbound.Tag] = append(edges[outbound.Tag], outbound.ProxyTag)
		}
		if outbound.Type != "" {
			for _, field := range []string{"detour", "default"} {
				if tag, ok := outbound.Extra[field].(string); ok {
					edges[outbound.Tag] = append(edges[outbound.Tag], tag)
				}
			}
			if list, ok := outbound.Extra["outbounds"].([]any); ok {
				for _, item := range list {
					if tag, ok := item.(string); ok {
						edges[outbound.Tag] = append(edges[outbound.Tag], tag)
					}
				}
			}
		} else {
			if proxy, ok := outbound.Extra["proxySettings"].(map[string]any); ok {
				if tag, ok := proxy["tag"].(string); ok {
					edges[outbound.Tag] = append(edges[outbound.Tag], tag)
				}
			}
			if stream, ok := outbound.Extra["streamSettings"].(map[string]any); ok {
				if socket, ok := stream["sockopt"].(map[string]any); ok {
					if tag, ok := socket["dialerProxy"].(string); ok {
						edges[outbound.Tag] = append(edges[outbound.Tag], tag)
					}
				}
			}
		}
	}
	visited, pending := map[string]bool{}, map[string]bool{}
	var walk func(string) error
	walk = func(tag string) error {
		if pending[tag] {
			return fmt.Errorf("custom_outbounds: outbound dependency cycle at %q", tag)
		}
		if visited[tag] {
			return nil
		}
		pending[tag] = true
		for _, next := range edges[tag] {
			if err := walk(next); err != nil {
				return err
			}
		}
		pending[tag] = false
		visited[tag] = true
		return nil
	}
	for tag := range edges {
		if err := walk(tag); err != nil {
			return err
		}
	}
	return nil
}

// Native routes are never translated between kernels. Unknown native fields
// remain intact; the selected core performs its full version-specific parsing.
func ValidateNativeRoutes(rules []map[string]any, kernel string, tags map[string]struct{}) error {
	for i, rule := range rules {
		path := fmt.Sprintf("custom_routes[%d]", i)
		if len(rule) == 0 {
			return fmt.Errorf("%s must be a nonempty object", path)
		}
		if kernel == "xray" {
			for _, key := range []string{"outbound", "action", "domain_suffix", "ip_cidr", "rules"} {
				if _, ok := rule[key]; ok {
					return fmt.Errorf("%s.%s is not an Xray native rule field", path, key)
				}
			}
			if value, ok := rule["outboundTag"]; ok {
				if err := checkOutboundReference(value, path+".outboundTag", tags, ""); err != nil {
					return err
				}
			} else if value, ok := rule["balancerTag"].(string); !ok || value == "" {
				return fmt.Errorf("%s requires outboundTag or balancerTag", path)
			}
		} else if err := validateSingboxRule(rule, path, tags, true); err != nil {
			return err
		}
	}
	return nil
}

func validateSingboxRule(rule map[string]any, path string, tags map[string]struct{}, action bool) error {
	for _, key := range []string{"outboundTag", "balancerTag", "localPort", "sourcePort", "inboundTag", "ip"} {
		if _, ok := rule[key]; ok {
			return fmt.Errorf("%s.%s is not a sing-box native rule field", path, key)
		}
	}
	if value, ok := rule["type"]; ok && value != "logical" && value != "default" {
		return fmt.Errorf("%s.type is not a sing-box rule type", path)
	}
	if action {
		if value, ok := rule["action"]; ok {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s.action must be a string", path)
			}
		}
		if value, ok := rule["outbound"]; ok {
			if err := checkOutboundReference(value, path+".outbound", tags, ""); err != nil {
				return err
			}
		} else if rule["action"] == nil || rule["action"] == "route" {
			return fmt.Errorf("%s requires outbound for route action", path)
		}
	}
	if nested, exists := rule["rules"]; exists {
		list, ok := nested.([]any)
		if !ok {
			return fmt.Errorf("%s.rules must be an array", path)
		}
		for i, item := range list {
			child, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%s.rules[%d] must be an object", path, i)
			}
			if err := validateSingboxRule(child, fmt.Sprintf("%s.rules[%d]", path, i), tags, false); err != nil {
				return err
			}
		}
	}
	return nil
}
