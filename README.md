# xboard-node-u

基于 [Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，支持 Xboard 面板的机器模式、单节点模式，以及无面板独立运行。

| 内核 | 当前版本 |
| --- | --- |
| sing-box | 1.14.4（含 Xboard 兼容补丁） |
| Xray-core | 26.11.9（含 Xboard 兼容补丁） |

当前版本为 **v2.4.1**，配套 Xboard-u v2.4.1。接入未提供批次流量上报的面板时，新节点仍可使用旧协议。

两套内核已嵌入程序，无需另装内核。[变更记录](CHANGELOG.md)

## 二次开发功能

[v2.4 - lastest 二次开发功能说明](docs/v2.4%20-%20lastest%20二次开发功能说明.md)\
[v1.0 - v2.3 二次开发功能说明](docs/v1.0%20-%20v2.3%20二次开发功能说明.md)

- v2.4.1 新增带回执的流量批次上报、机器子节点切换 XHTTP 时自动改用 Xray，并改进撤权同步、证书续签和 VLESS TCP 限速。
- v2.3.0 新增 [HY2 端口跳跃与自动转发](docs/HY2-PORT-HOPPING.md)，两种内核、三种运行模式共用范围管理及规则清理。
- v2.2.1 同步 Xray-core 官方最新正式版 **26.11.9**。
- v1.1.0 新增 Xray Fallback 回落功能。
- v1.0.2 同步 **Xray-core v26.10.31 紧急安全更新**。
- v1.0.1 新增 Alpine Linux / OpenRC 独立安装器；无 systemd 的 LXC 可直接安装。两种系统统一使用 `xbctl` 管理。
- v1.0.0 整合 sing-box 和 Xray 最新版内核，支持机器模式、单节点接入面板及无面板裸核运行。

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

[HY2 端口跳跃与自动转发](docs/HY2-PORT-HOPPING.md) · [独立 HY2 范围示例](examples/standalone-hy2-port-hopping.yml.example)

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
