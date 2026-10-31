# Xray v26.10.31 安全更新与本地验证

验证日期：2026-11-01。本次更新及测试全部在本地完成，未连接 VPS。

## 更新来源

- [官方 v26.10.31 公告](https://github.com/XTLS/Xray-core/releases/tag/v26.10.31)。
- 固定上游提交：`a4a3dff5b20c2faae567b742c172c7b95e75c083`。
- Go 模块规范版本：`v1.260327.1-0.20261031132019-a4a3dff5b20c`。
- [上游默认关闭 MPTCP 的修改](https://github.com/XTLS/Xray-core/pull/7291)。

公告说明本次更新默认关闭 MPTCP，以减少相关 Linux TCP 内核漏洞的攻击面。该措施不替代 Linux 内核修复；节点软件升级不能保证系统已消除该漏洞。

## 项目改动

- 更新 Xray 下载脚本中的版本、固定提交、补丁名称和 Go 模块依赖。
- 兼容补丁更名为 `patches/xray-core-26.10.31.patch`。原有补丁可直接应用，保留用户限速、Mux 缓冲读取、初始化配置工厂及 Hysteria 会话并发修复。
- protobuf 跟随上游更新到 v1.36.12；sing-box 保持 1.14.4-xboard.1。
- 健康检查端口改用明确调用 `SetMultipathTCP(false)` 的监听器，即使 Go 的默认值受环境变量影响，也使用普通 TCP。
- 保留上游用户显式配置 `sockopt.tcpMptcp: true` 的能力；本次保证默认行为，不覆盖用户主动启用选项。

## 测试隔离

从项目已跟踪文件建立独立副本，在该副本准备内核、编译和测试。最终只将代码、补丁、依赖清单和文档变更同步回正式项目。

二进制、测试配置、测试密钥、日志及编译缓存均位于项目目录外。Linux 容器中的项目源码以只读方式挂载，部署文件写入容器自己的 `/etc`、`/tmp` 和 `/var`，完成后移除容器。

## 已验证

- Go 1.27.3：全项目完整构建 tags 的测试通过，并在本机 Linux 容器完成 `-race -count=1` 检查。
- 真实代理回归：VMess、VLESS、Trojan TLS、传统 SS、SS2022、VLESS WS/gRPC/XHTTP、Mux、XUDP、Hysteria2；含用户更新、流量统计、动态限速、同端口重载与失败回滚。共享依赖变化后的 sing-box 回归一并通过。
- 使用新版节点和 xbctl 的 Alpine/OpenRC 21 项集成检查通过，含安装、升级、机器与单节点两种面板模式的真实 HTTP 代理、服务重启、崩溃恢复、配置保留、升级失败回滚和卸载。
- 无面板独立模式真实代理 HTTP 200，健康检查 HTTP 200，结束进程并重新启动后再次通过。
- systemd 安装器的原有本机模拟向导检查通过；未在真实 systemd 系统重新安装服务。
- Linux amd64/arm64 节点与 xbctl 编译通过。arm64 在本地模拟容器启动并显示 Xray 26.10.31；完整代理与部署检查在 amd64 执行。

## MPTCP 行为验证及边界

Linux socket 回归测试在 `GODEBUG=multipathtcp=2` 下检查 Xray 默认监听、空 socket 选项监听及健康检查监听，socket 的 `SO_PROTOCOL` 均为普通 TCP。

本机 Docker Linux 内核未提供 MPTCP，因此仅观察 socket 的最终类型不足以区分“主动关闭”与“尝试后回退”。额外用 `strace` 观测实际节点进程的 socket 系统调用，并使用同环境下的 v1.0.1 旧版作为正对照：

| 检查对象 | 实际代理与健康检查 | 尝试创建 MPTCP socket |
| --- | --- | --- |
| v1.0.1（Xray 26.10.29） | HTTP 200 | 是 |
| 本次构建（Xray 26.10.31） | HTTP 200 | 否 |
| 本次构建重新启动后 | HTTP 200 | 否 |

该测试验证了新旧程序的监听行为差异，没有运行漏洞利用，也不代表验证了宿主 Linux 内核的漏洞修复。尚未在支持 MPTCP 的另一台真实主机上验证协商行为。

本次未新增公网、长时间压力、REALITY/Vision 或 VLESS Encryption 端到端测试；此前的验证边界继续适用。上述本地验证使用开发构建，正式 v1.0.2 程序由对应标签的 GitHub Actions 构建、检查和发布。
