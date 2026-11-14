# xboard-node-u

基于 [Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，支持 Xboard 面板的机器模式、单节点模式，以及无面板独立运行。

| 内核 | 当前版本 |
| --- | --- |
| sing-box | 1.14.4（含 Xboard 兼容补丁） |
| Xray-core | 26.11.9（含 Xboard 兼容补丁） |

当前版本：**v2.2.1**，兼容配套面板 xboard-u v2.2.0。

两套内核已嵌入程序，无需另装内核。[变更记录](CHANGELOG.md)

## 二次开发新功能（详见 [二次开发功能说明](docs/二次开发功能说明.md)）

- v2.2.1 同步 Xray-core 26.11.9，保留 Xboard 兼容补丁；Sing-box 保持 1.14.4，详见 [内核升级说明](docs/二次开发功能说明.md#xray-内核升级v221)。
- v2.2.0 修复 HY2 带宽映射，新增自动 BBR / Brutal 协商及 Masquerade，详见 [HY2 配置说明](docs/HYSTERIA2.md)。
- v2.1.0 修复 Xray 并发访问、停止时监听释放及机器模式重连，优化设备快照与在线心跳。
- v2.0.0 配套面板 CDN 分离，补齐 Sing-box 传输参数映射及 Xray WebSocket Host 迁移。
- v1.1.3 支持 Sing-box / Xray 原生自定义出站和路由，兼容旧面板出站格式。
- 新增 Xray Fallback，支持面板下发及无面板独立配置；TLS / Reality 均使用原生协议层回落，保留 Reality 目标。
- 更新双内核，支持 Alpine/OpenRC 安装和统一 `xbctl` 管理。

## Linux systemd 安装

适用于使用 systemd 的 Debian、Ubuntu 等 Linux 系统。

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install.sh | sudo bash -s -- --interactive
```

## Alpine Linux / OpenRC 安装

适用于使用 OpenRC 的 Alpine Linux，包括无 systemd 的 Alpine LXC。以 root 执行：

```sh
apk add --no-cache curl ca-certificates && curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install-alpine.sh -o /tmp/install-alpine.sh && sh /tmp/install-alpine.sh --interactive
```

## 配置示例

[单节点](examples/node.yml.example) · [机器模式](examples/machine.yml.example) · [无面板独立运行](examples/standalone.yml.example) · [多实例](config.yml.example)

独立节点配置：[SS](examples/standalone.yml.example) · [VLESS + Reality](examples/standalone-vless-reality.yml.example) · [HY2](examples/standalone-hy2.yml.example) · [VLESS + TLS 回落](examples/standalone-vless-fallback.yml.example) · [Xray Reality 回落](examples/standalone-xray-reality-fallback.yml.example)

支持通过服务端本地 `kernel.custom_config` 自定义 DNS、出站和路由等高级配置，适用于机器、单节点及独立模式，详见 [自定义内核配置](docs/CUSTOM-CONFIG.md)。

## 常用管理命令

以下命令以 root 执行，普通用户按需添加 `sudo`：

```sh
xbctl help       # 查看帮助
xbctl status     # 查看运行状态
xbctl version    # 查看管理工具版本
xbctl list       # 查看实例列表
xbctl start      # 启动服务
xbctl stop       # 停止服务
xbctl restart    # 重启服务
xbctl log        # 查看日志
xbctl upgrade    # 升级到最新正式版
```

## 无面板独立运行（裸核运行）

从本地配置读取节点和用户，无需连接面板。配置替换、客户端参数及与机器模式共存的写法见 [独立运行指南](docs/STANDALONE.md)。

## 自定义路由与出站

以下两项在 **Xboard 面板的节点配置中设置**，由面板下发到节点端；不是在本地服务器的 `config.yml` 中填写这些文档的 JSON 示例。

- [自定义路由](docs-custom-routes.md)
- [自定义出站](docs-custom-outbounds.md)

## 服务端本地高级配置

通过 `kernel.custom_config` 引用 JSON 或 YAML 文件，在服务端自定义 DNS、出站、路由等内核配置。支持机器模式、面板单节点模式及无面板独立运行。配置位置、共享方式、DNS 主备示例和字段合并规则见 [自定义内核配置指南](docs/CUSTOM-CONFIG.md)。
