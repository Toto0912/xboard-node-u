# sing-box 1.14.4 upgrade

## Source and scope

The node embeds upstream sing-box v1.14.4, commit
`c005f35300ce76427dc285481ec5695f5b179cd2`, plus the compatibility patch in
`patches/sing-box-1.14.4.patch`. In this combined build, Xray is upgraded separately;
see `docs-xray-upgrade.md`.
`xboard-node -v` prints the patched sing-box version (`1.14.4-xboard.1`).

The patch retains live user updates and Mieru support from the original fork,
ports Mieru cleanup to the new Scope lifecycle, uses stable user identities
instead of changing array indexes during hot updates, and makes HTTP/SOCKS/Naive
authenticator replacement atomic. Empty user updates reject authentication.

## Build

Requirements: Go 1.27 or later, Git, and network access to GitHub and Go modules.

```sh
go run ./scripts/prepare-singbox.go
go run ./scripts/prepare-xray
go run ./scripts/prepare-transports
go test -tags 'with_quic with_utls with_wireguard with_clash_api' ./internal/... ./cmd/... ./scripts/...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags 'with_quic with_utls with_wireguard with_clash_api' -o xboard-node ./cmd/xboard-node
```

`make build`, `make build-linux`, `make build-linux-arm64`, `make test`, and the
Dockerfile prepare the pinned source automatically. `make test` also runs the
race detector (requires a C compiler). The generated `third_party/sing-box`
checkout is ignored; the versioned patch and preparation program reproduce it.
Preparation verifies the upstream commit and refuses to overwrite unexpected
local changes. Do not remove the preparation step from CI or Docker builds.

The initial Linux race-detector run found concurrent credential-table access
in the pinned sing-vmess and sing-quic dependencies. Additional versioned
patches capture an immutable VMess user generation and synchronize TUIC user
and password lookup. `scripts/prepare-transports` prepares these dependencies;
no shared Go module cache files are modified.

## Runtime behavior

VMess, VLESS, Shadowsocks multi-user, Trojan, Hysteria2, TUIC, AnyTLS, SOCKS and
HTTP user updates use live credential replacement. Other protocols fall back
to rebuilding the instance instead of silently ignoring changes.

Node configuration, routing, outbound or certificate changes rebuild the full
instance using the new lifecycle API. Existing connections can be interrupted.
Invalid options are rejected before shutdown; a startup failure attempts to
restore the previous configuration. Cumulative traffic counters survive reload.
This replaces the old fork's unsupported Router.UpdateRules/inbound removal API.

## Local validation

The upgrade tests start real servers and clients on loopback and cover TCP echo,
adding/removing users, keeping existing connections alive during user updates,
traffic attribution, same-port reload, invalid configuration and occupied-port
rollback. Protocol coverage: VMess, VLESS, Shadowsocks, SOCKS, HTTP, Trojan,
AnyTLS, Hysteria2 and TUIC (the last two require `with_quic`). An additional test
checks that removing the last SOCKS user does not enable anonymous access.
Existing tracker/limiter tests cover byte counting, device gating and throttling.

The tested local toolchain is Go 1.27.3 on Windows amd64. Linux amd64 and arm64
executables are cross-compiled. Linux amd64 VPS deployment and panel/node
integration have since been verified, and the operator confirmed public access.
UDP application traffic, Mieru/Hysteria1/Naive interoperability, performance,
and a local race-detector run are not covered by these results. Docker builds
are configured but have not been executed locally (Docker daemon unavailable).

Keep the first deployment on a test node. Use dedicated free ports and avoid
changing panel listeners. On a VPS with burst CPU limits, upload locally built
executables and run short functional checks; do not compile or benchmark there.
