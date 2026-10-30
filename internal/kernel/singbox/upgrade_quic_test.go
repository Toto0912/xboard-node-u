//go:build with_quic

package singbox

func init() {
	upgradeProtocols = append(upgradeProtocols, "hysteria2", "tuic")
}
