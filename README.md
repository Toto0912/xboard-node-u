# xboard-node-u

基于 [Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，支持 Xboard 面板的机器模式、单节点模式，以及无面板独立运行。

| 内核 | 当前版本 |
| --- | --- |
| sing-box | 1.14.4（含 Xboard 兼容补丁） |
| Xray-core | 26.10.31（含 Xboard 兼容补丁） |

两套内核已嵌入程序，无需另装内核。[变更记录](CHANGELOG.md)

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

独立节点配置：[SS](examples/standalone.yml.example) · [VLESS + Reality](examples/standalone-vless-reality.yml.example) · [HY2](examples/standalone-hy2.yml.example)

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
