# Xray v26.10.29 本地升级记录

本分支包含 sing-box 1.14.4-xboard.1 和 Xray 26.10.29。2026-10-30 已部署到测试 VPS，
用户已确认公网访问正常。发布项目不包含服务器配置或访问凭据。
Xray 来源固定为官方 v26.10.29 提交 `0afec66085a85191e9dd2a4a6646c7c9a94688d1`，
并应用 `patches/xray-core-26.10.29.patch`。
官方未提供对应的 v1.* 模块标签，因此 go.mod 使用 Go 解析该提交得到的规范伪版本
`v1.260327.1-0.20261029083850-0afec66085a8`。这不是回退到三月份内核。
运行 `xboard-node -v` 可查看两套内核的真实版本。

## 改动

- 迁移原 fork 的用户限速、Mux 包装读取器兼容补丁及相关测试。
- 为核心提供仅用于初始化阶段的配置工厂装饰接口，移除节点端的 go:linkname/unsafe 访问。
- 适配 SS2022 内存账户的字节密钥，并与启动配置中的密钥生成规则保持一致。
- 已移除的 Shadowsocks none/plain 加密不再错误回落到其他加密算法。
- 限速按 burst 分段等待，避免大批缓冲区超过 burst 时直接断开；每次写入重新读取当前限速，
  已建立连接也能在下一次写入采用新的限速或解除限速。
- 将构建工具链升级到 Go 1.27，Docker 和 Makefile 自动准备两套固定版本的内核补丁。
- VPS 联调补充：机器 WS 事件按各节点实际内核解析，避免混合内核 XHTTP 配置被按 sing-box 拒绝。
- VPS 联调补充：Xray 同端口配置重载先释放旧监听，启动失败尝试回滚，保留累计流量；配置重载可中断现有连接。
- VPS 联调补充：Hysteria2 协议设置明确生成 version=2，并支持 Hysteria 用户账户热更新。
- GitHub Linux 并发检查补充：Hysteria 会话管理器在锁内检查关闭状态，单会话关闭标记改用原子访问。

## 本地验证

- Windows amd64，Go 1.27.3。
- 全项目 `internal/...`、`cmd/...`、`scripts/...` 测试通过（使用完整构建 tags）。
- 本地真实 TCP 代理：VMess、VLESS、Trojan TLS、传统 SS、SS2022、VLESS WS、gRPC、XHTTP、Mux。
- 上述场景验证用户新增、用户移除、剩余用户连接持续可用、用户上下行流量计数。
- 真实 VLESS XUDP 回环包收发及流量计数通过。
- Xray 补丁涉及的 dispatcher、Mux、bandwidth、common 测试通过；覆盖取消限速等待、
  超过 burst 的数据批次、动态限速变更及 Mux 会话回收。
- 共享依赖升级后的 sing-box 真实连接回归通过。
- Linux amd64、arm64 交叉编译成功；Linux AMD64 VPS 已验证机器、node、standalone 三种模式。
- 默认机器内核明确设置为 xray；机器 XHTTP、单节点 XHTTP、无面板 VLESS TCP 均完成 HTTPS 请求。
- 原 Hysteria2 节点在 Xray 下已用 sing-box 客户端通过公网完成 HTTPS 请求。
- 本地新增同端口重载、端口冲突回滚和混合内核 WS 事件路由测试。

## 构建

需要 Go 1.27 或更新版本、Git 和依赖下载网络。

```sh
go run ./scripts/prepare-singbox.go
go run ./scripts/prepare-xray
go run ./scripts/prepare-transports
go test -tags 'with_quic with_utls with_wireguard with_clash_api' ./internal/... ./cmd/... ./scripts/...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags 'with_quic with_utls with_wireguard with_clash_api' -o xboard-node ./cmd/xboard-node
```

`make build`、`make build-linux`、`make build-linux-arm64`、`make test`、Dockerfile 已接入两套内核及传输依赖的准备步骤。
`make test` 额外启用 race detector，需要 C 编译器；本机尚未执行 race 检查。
`third_party/` 是生成目录，源码包包含完整补丁和可重建脚本。脚本验证上游提交，拒绝覆盖意外修改。

## 兼容性边界

- 新版移除了 TLS allowInsecure。测试改用十六进制 SHA256 证书指纹 pinnedPeerCertSha256。
  用户提供的自定义原生 Xray 配置若使用旧参数，需要按上游规则迁移。
- Freedom 增加了默认私有地址阻止行为。测试只在测试出站中明确允许 127.0.0.1；生产默认保护未放开。
- 上游对 VMess、Trojan 等旧功能会发出弃用提示。本版保留当前仍可用的功能。
- 本轮目标是升级内核并保持已有功能，不代表面板自动支持全部新增字段或新协议。
- REALITY/Vision、VLESS Encryption 新参数、负载和长时间稳定性未完成端到端验证。
  Mux/XUDP 的本地通过不能代替复杂公网环境的长时间测试。
- 计费跨进程重启连续性和多机器规模运行没有新增集成验证。
- 当前面板关闭 WebSocket，远程配置更新使用 REST；WS 混合内核修复有本地回归测试，尚未重新开启面板 WS 做远程复测。
- 长期负载尚未验证；发布和验证范围见 `docs/RELEASE.md` 与 `docs/VALIDATION.md`。
