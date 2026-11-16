//go:build linux

package porthopping

import (
	"fmt"
	"net/netip"
	"strings"
)

func families(spec *Spec) []string {
	if spec == nil {
		return nil
	}
	ip := netip.MustParseAddr(spec.Listen)
	if spec.Listen == "::" {
		return []string{"iptables", "ip6tables"}
	}
	if ip.Is4() {
		return []string{"iptables"}
	}
	return []string{"ip6tables"}
}

func unionFamilies(r *record, spec *Spec) []string {
	result := []string{}
	for _, s := range []*Spec{r.Active, r.Pending, spec} {
		for _, family := range families(s) {
			if !has(result, family) {
				result = append(result, family)
			}
		}
	}
	return result
}

func has(items []string, wanted string) bool {
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

func tableName(r *record) string { return "xbhy2_" + r.Owner }
func chainName(r *record) string { return "XBHY2_" + r.Owner }

func nftExists(r *record) (bool, error) {
	output, err := command("", "nft", "list", "tables")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "table inet "+tableName(r) {
			return true, nil
		}
	}
	return false, nil
}

func nftScript(r *record, spec *Spec, existing bool) string {
	name := tableName(r)
	script := ""
	if existing {
		script += fmt.Sprintf("delete table inet %s\n", name)
	}
	if spec == nil {
		return script
	}
	address := "fib daddr type local"
	ip := netip.MustParseAddr(spec.Listen)
	if spec.Listen == "0.0.0.0" {
		address = "meta nfproto ipv4 " + address
	} else if !ip.IsUnspecified() {
		family := "ip"
		if ip.Is6() {
			family = "ip6"
		}
		address = family + " daddr " + ip.String()
	}
	return script + fmt.Sprintf("add table inet %s\n"+
		"add chain inet %s prerouting { type nat hook prerouting priority dstnat; policy accept; }\n"+
		"add rule inet %s prerouting %s udp dport %d-%d counter redirect to :%d comment \"xboard-node HY2\"\n",
		name, name, name, address, spec.Start+1, spec.End, spec.Start)
}

func iptArgs(args ...string) []string {
	return append([]string{"-w", "5", "-t", "nat"}, args...)
}

func jump(r *record) []string {
	return []string{"PREROUTING", "-p", "udp", "-m", "comment", "--comment", "xbhy2:" + r.Owner, "-j", chainName(r)}
}

func iptExists(binary string, r *record) (bool, error) {
	_, err := command("", binary, iptArgs("-S", chainName(r))...)
	if err == nil {
		return true, nil
	}
	if _, check := command("", binary, iptArgs("-S")...); check != nil {
		return false, check
	}
	return false, nil
}

func removeFamily(binary string, r *record) error {
	exists, err := iptExists(binary, r)
	if err != nil || !exists {
		return err
	}
	for {
		if _, err := command("", binary, iptArgs(append([]string{"-C"}, jump(r)...)...)...); err != nil {
			break
		}
		if _, err := command("", binary, iptArgs(append([]string{"-D"}, jump(r)...)...)...); err != nil {
			return err
		}
	}
	if _, err := command("", binary, iptArgs("-F", chainName(r))...); err != nil {
		return err
	}
	_, err = command("", binary, iptArgs("-X", chainName(r))...)
	return err
}

func applyRules(r *record, spec *Spec) error {
	if r.Backend == "nft" {
		exists, err := nftExists(r)
		if err != nil {
			return err
		}
		script := nftScript(r, spec, exists)
		if script == "" {
			return nil
		}
		_, err = command(script, "nft", "-f", "-")
		return err
	}
	for _, binary := range unionFamilies(r, spec) {
		if !has(families(spec), binary) {
			if err := removeFamily(binary, r); err != nil {
				return err
			}
			continue
		}
		name := chainName(r)
		destination := "-m addrtype --dst-type LOCAL"
		if ip := netip.MustParseAddr(spec.Listen); !ip.IsUnspecified() {
			destination = "-d " + ip.String()
		}
		script := fmt.Sprintf("*nat\n:%s - [0:0]\n-F %s\n-A %s %s -p udp --dport %d:%d -j REDIRECT --to-ports %d\nCOMMIT\n",
			name, name, name, destination, spec.Start+1, spec.End, spec.Start)
		if _, err := command(script, binary+"-restore", "--wait", "5", "--noflush"); err != nil {
			return err
		}
		if _, err := command("", binary, iptArgs(append([]string{"-C"}, jump(r)...)...)...); err != nil {
			if _, err := command("", binary, iptArgs(append([]string{"-A"}, jump(r)...)...)...); err != nil {
				return err
			}
		}
	}
	return nil
}

func removeRules(r *record) error { return applyRules(r, nil) }
