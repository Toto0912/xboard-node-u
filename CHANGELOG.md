# 变更记录

## 未发布

- 补充 `kernel.custom_config` 服务端高级配置文档，说明三种运行模式、实例继承与文件共享、双内核字段合并规则、DNS 主备及本地出站和路由示例；在 README 和相关指南增加入口。本项仅补充既有功能说明，未改变程序或版本号。

## v2.1.0 — 2026-11-06

- 2026-11-07 修复 Xray Reality 将面板分开填写的伪装站点 `dest` 和端口 `server_port` 组合为完整目标地址；空目标回退到 SNI，已包含端口的目标保持优先，SNI 和协议层 fallback 不变。适用于机器、单节点及独立模式。
- 修正 Xray 并发准入与连接状态读取的共享映射访问，停止服务时同步释放监听端口。
- 设备上报使用独立完整快照，持续在线也定期刷新；最后一个设备断开时明确上报空快照，失败后继续使用最新状态重试。
- 修正机器模式配置副本将空列表变为 null，导致相同配置在 WebSocket 重连时误触发内核重启的问题。
- 清理设备跟踪器中失效的去重和快照缓冲逻辑；三种部署模式无需增加配置，内核版本保持不变。

## v2.0.0 — 2026-11-03

- 2026-11-05 修复 sing-box 多实例共存时，一个实例重建关闭共享 D-Bus 连接，导致其他实例 DNS 解析失败的问题。

- 配套 xboard-u CDN 分离，保持既有节点通信结构及三种部署模式。
- 补齐 Sing-box gRPC 超时、HTTP 和 HTTPUpgrade 参数映射。
- Xray 将旧 WebSocket headers.Host 映射为独立 host，明确拒绝已移除的旧 HTTP/H2/H3 和 V2Ray QUIC。
- 增加本地 TLS 反代 XHTTP、Reality 与协议层回落下行流量验证。
- 两端版本统一为 v2.0.0；内核仍为 Sing-box 1.14.4 与 Xray 26.10.31。

## v1.1.3 — 2026-11-03

- 支持面板下发 Sing-box 原生出站，保留 type、detour、TLS、transport 等顶层及嵌套字段；保留 Xray 原生 streamSettings、proxySettings、mux 等字段。
- 兼容旧 protocol/settings/proxy_tag 出站格式；Sing-box 链式出站转换为原生 detour。
- 原生路由按实际内核检查格式和出站引用，检查出站依赖循环，错误配置明确拒绝；原生规则不做跨内核转换。
- 面板保存、重开、REST / WebSocket 下发保留空对象 {}，增加格式提示和原生配置文档；不新增数据库迁移。
- 面板与节点端版本统一为 v1.1.3，内核版本不变。


## v1.1.2 — 2026-11-02

- 独立模式的 `standalone.node.fallbacks` 支持 Xray VLESS / Trojan TCP/RAW + TLS / Reality，统一使用原生列表；不增加单目标独立字段。
- 面板下发的可选回落在 TLS / Reality 下均生成协议层 `fallbacks`，不再覆盖 Reality 的 dest，保留 SNI、密钥、Short ID 和 Flow。
- 普通浏览器 / Reality 鉴权失败仍转发到原 HTTPS 目标，Reality 握手通过后的协议首包或认证失败才使用原生回落；保留合法代理和 PROXY protocol。
- Reality 不注入普通 TLS ALPN 设置；条件按内核实际协商值匹配。VLESS 额外加密在 TLS、Reality 下均与回落不兼容。
- 修复 Xray 在未显式填写 dest 时忽略面板 Reality `server_port` 的问题，显式 dest 仍优先。
- 保持 sing-box 忽略面板回落字段和客户端订阅隔离；同步示例、升级说明和两端 v1.1.2 版本。

## v1.1.0 — 2026-11-01

- 无面板独立模式新增 `standalone.node.fallbacks`，支持 Xray VLESS / Trojan TCP+TLS 的默认、SNI、ALPN、路径回落及 PROXY protocol。
- 独立模式对不兼容的内核、协议、传输、Reality、VLESS decryption 和无效回落参数给出明确错误。
- 增加 VLESS + TLS 回落配置示例和裸核运行说明。
- 支持 xboard-u 下发的可选 `xray_fallback`，覆盖机器 / 单节点的 REST 拉取及 WS 更新；Xray TCP/RAW + TLS 使用普通回落，Reality 覆盖伪装目标。
- sing-box 忽略面板回落字段，不影响启动或因该字段变化触发多余重载；旧面板、空值及无效输入保持原行为。

## v1.0.2 — 2026-11-01

- 同步官方 Xray v26.10.31 安全更新，固定提交 `a4a3dff5b20c2faae567b742c172c7b95e75c083`，默认关闭 Xray TCP 监听的 MPTCP。
- 节点健康检查监听同样显式关闭 MPTCP，避免受 Go 隐式默认值或 `GODEBUG` 设置影响。
- 将现有 Xboard 限速、Mux、配置注册和 Hysteria 并发兼容补丁迁移至新固定版本，更新 protobuf 至 v1.36.12。
- 补充 Linux socket 协议回归测试；本地全项目 race、协议连接、三种部署模式与 OpenRC 安装升级检查通过。

## v1.0.1 — 2026-10-30

- 新增独立的 Alpine / OpenRC 安装器 `install-alpine.sh`，README 在 systemd 安装后提供一键命令。
- `xbctl` 自动识别 systemd/OpenRC，统一状态、启停、日志、自启、绑定、升级和卸载管理。
- Alpine 安装及升级校验同一版本的节点程序和 xbctl，保存安装器，并支持失败回滚与旧管理工具迁移。
- 兼容旧看门狗卸载后的残留文件，不依据过期 PID 文件误杀其他进程；保留原配置、凭据和其他启动任务。
- 修复 xbctl 重写绑定时丢失旧配置内嵌令牌的问题。
- 添加 Alpine 本地/CI 集成测试，正式 Release 增加 Alpine 安装器附件及 SHA256 清单项。

## v1.0.0 — 2026-10-30

- 合并 sing-box 1.14.4 与 Xray 26.10.29 升级；提供固定提交、完整补丁及重建脚本。
- 迁移 sing-box 用户热更新、Mieru 生命周期、HTTP/SOCKS/Naive 认证替换和配置重载逻辑。
- 迁移 Xray 用户限速、Mux 包装读取器、SS2022 账户及 Hysteria2 配置和用户更新逻辑。
- 移除节点端对 Xray 配置工厂注册表的 unsafe/go:linkname 访问。
- 修复机器 WS 事件使用全局内核解析配置的问题，改用对应节点的已解析内核。
- 修复 Xray 同端口配置重载，补充启动失败回滚和累计流量保留。
- 补充两套内核的真实协议连接、用户增删、流量统计和配置重载回归测试。
- 统一 Go 1.27 构建配置，整理三种部署模式的示例和说明。
- 安装器和 xbctl 支持自己的发布仓库；GitHub 工作流根据当前仓库生成 Release 与镜像地址。
- 首次 Linux race 检查发现并修复 VMess/TUIC 用户表并发访问和 Xray Hysteria 会话关闭状态的竞争；使用固定提交的依赖补丁，不禁用并发检查。
- 新增一行命令启动的交互安装向导，支持机器/单节点与两套内核，密钥隐藏输入；README 优先展示 systemd 安装。
- 多架构 Docker 构建改用本机交叉编译，避免在模拟环境中编译完整 Go 程序。

本次升级已在测试 VPS 验证，用户已确认公网节点访问正常。
