# xboard-node-u

基于 [Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，支持 Xboard 面板的机器模式、单节点模式，以及无面板独立运行。

| 内核 | 当前版本 |
| --- | --- |
| sing-box | 1.14.4（含 Xboard 兼容补丁） |
| Xray-core | 26.10.31（含 Xboard 兼容补丁） |

两套内核已嵌入程序，无需另装内核。[下载正式版](https://github.com/Toto0912/xboard-node-u/releases/latest) · [变更记录](CHANGELOG.md)

当前源码已同步 Xray v26.10.31 安全更新，默认关闭 Xray 入站及节点健康检查监听的 MPTCP。这是减少攻击面的措施，服务器仍应更新 Linux 内核。安装脚本下载的是已发布 Release，实际内核版本请用 `xboard-node -v` 确认。[更新说明与本地验证](docs/XRAY-SECURITY-2026-10-31.md)

## Linux systemd 安装

支持使用 systemd 的 Linux，自动识别 amd64 / arm64。复制下面一行到服务器终端运行：

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install.sh | sudo bash -s -- --interactive
```

按提示选择**机器模式或单节点模式**、**sing-box 或 Xray**，然后填写面板地址、ID 和密钥，即可自动下载程序、生成配置并启动服务。密钥输入时不会显示。

- 机器模式：填写面板中该机器的 ID 和**机器令牌**。
- 单节点模式：填写节点 ID 和面板的**通信密钥**。

默认内核为 sing-box；选择 Xray 时，安装器会明确写入 `kernel.type: xray`。
配置保存在 `/etc/xboard-node/config.yml`，服务名为 `xboard-node.service`。

### 常用管理命令

```bash
sudo xbctl status            # 查看状态
sudo xbctl service restart   # 重启
sudo xbctl upgrade           # 升级到最新正式版
```

需要无人值守安装、指定版本或使用本地程序时，见 [详细安装说明](docs/INSTALL.md)。

## Alpine Linux / OpenRC 安装

适用于使用 OpenRC 的 Alpine Linux，包括没有 systemd 的 LXC。以 **root** 复制运行：

```sh
apk add --no-cache curl ca-certificates && curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install-alpine.sh -o /tmp/install-alpine.sh && sh /tmp/install-alpine.sh --interactive
```

按提示填写即可，默认机器模式、sing-box。自动安装程序和 `xbctl`，配置 OpenRC 后台运行、异常重启和开机自启。
已有配置及密钥默认保留；旧服务已停止但残留脚本/PID 时，会自动备份并清理。

管理命令与 systemd 相同（Alpine 上以 root 执行）：

```sh
xbctl status
xbctl service restart
xbctl service logs
xbctl upgrade
```

独立安装器为 [install-alpine.sh](install-alpine.sh)，无需 Bash，也无需安装 systemd。
需使用 v1.0.1 及以上版本；v1.0.0 不包含此安装器。
[详细安装、迁移与离线说明](docs/INSTALL-ALPINE.md)。

## 功能与配置

- 自动同步面板节点配置和用户，支持 WebSocket 推送及 REST 轮询。
- 支持多实例、多面板、用户热更新、限速、设备限制和流量统计。
- 协议和参数支持取决于面板与所选内核，详见 [验证范围](docs/VALIDATION.md)。

配置示例：[单节点](examples/node.yml.example) · [机器模式](examples/machine.yml.example) · [无面板独立运行](examples/standalone.yml.example) · [多实例](config.yml.example)

选择内核时设置 `kernel.type: singbox` 或 `kernel.type: xray`，机器模式也使用这一设置。协议兼容性可能触发内核选择调整，请以启动日志为准。

### 无面板独立运行（standalone）

独立模式直接从本地配置读取节点参数和用户，不需要面板地址、通信密钥或机器 ID，也不会连接面板、同步用户或向面板上报流量。支持 sing-box 和 Xray，具体协议和参数需与所选内核兼容。

当前一键安装器的交互菜单只提供面板机器模式和单节点模式，不会自动生成 standalone 配置。以下步骤适用于已安装程序和服务的服务器；手动下载程序时，也可使用 `xboard-node -c /path/to/config.yml` 前台运行。

修改前先备份配置。以下服务器管理命令以 root 执行，普通用户请按需添加 `sudo`：

```sh
cp -p /etc/xboard-node/config.yml "/etc/xboard-node/config.yml.bak.$(date +%Y%m%d-%H%M%S)"
```

**只运行一个独立节点**时，可将 `/etc/xboard-node/config.yml` 的内容完整替换为以下示例。已有机器或面板节点需要继续运行时，请使用下一节的多实例写法。

```yaml
kernel:
  type: singbox
  config_dir: /var/lib/xboard-node/standalone-ss
  log_level: warn

standalone:
  enabled: true
  node:
    protocol: shadowsocks
    listen_ip: "0.0.0.0"
    server_port: 12345
    cipher: chacha20-ietf-poly1305
  users:
    - id: 1
      # 此协议下，uuid 字段用作 Shadowsocks 密码
      uuid: "REPLACE_WITH_YOUR_SHADOWSOCKS_PASSWORD"

log:
  level: info
  output: stdout

health_port: 0
```

- 将 `uuid` 中的占位文本替换为实际密码，客户端使用完全相同的密码和加密方式。对本例的传统 Shadowsocks 加密方式，这里不要求填写 UUID 格式的字符串；这不代表 VLESS、VMess 或 SS2022 的凭据规则相同。
- `listen_ip: "0.0.0.0"` 表示监听所有 IPv4 接口。客户端填写服务器的公网 IP 或域名，服务端通常无需将公网 IP 写入监听地址，尤其是在云平台 NAT 环境中。
- `server_port: 12345` 是示例端口，可自行修改；服务器防火墙和云平台安全组需放行对应的 TCP、UDP 端口。
- `health_port: 0` 表示关闭健康检查监听，与代理端口无关。

保存后执行：

```sh
xbctl restart
xbctl log
```

上述配置为 xboard-node-u 的 YAML 配置，并非原生 sing-box JSON 配置。更多字段可参考 [独立运行示例](examples/standalone.yml.example) 和 [配置定义](internal/config/standalone.go)。

### 已有机器模式，追加独立节点（多实例共存）

如果现有配置已经以 `instances:` 开头，保留原有机器实例，在同一个列表末尾添加独立实例即可。**新实例的 `- id:` 必须与原有的 `- id:` 对齐，不要再添加第二个 `instances:`，也不要把单实例示例直接粘贴到文件最外层或机器实例内部。** 缩进使用空格，不要使用 Tab。

以下展示完整结构，面板域名、机器 ID、令牌环境变量名、实例名、密码和端口均为示例。实际操作时保留自己的第一个实例，只追加第二个实例；不要使用这些示例值覆盖已有机器配置。

```yaml
instances:
  # 原有机器实例：保留实际配置和已有的 token_env
  - id: panel-machine-example
    panel:
      url: https://panel.example.com
    machine:
      machine_id: 1
      token_env: XBOARD_NODE_MACHINE_TOKEN
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/panel-machine-example
      log_level: warn
    log:
      level: info
      output: stdout
    health_port: 0

  # 新增独立实例：与上面的 - id: 对齐
  - id: standalone-ss-example
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/standalone-ss-example
      log_level: warn
    standalone:
      enabled: true
      node:
        protocol: shadowsocks
        listen_ip: "0.0.0.0"
        server_port: 12345
        cipher: chacha20-ietf-poly1305
      users:
        - id: 1
          uuid: "REPLACE_WITH_YOUR_SHADOWSOCKS_PASSWORD"
    log:
      level: info
      output: stdout
    health_port: 0
```

两个实例由同一个 xboard-node 服务运行：第一个继续从面板接收配置，第二个只使用本地配置。也可以为各实例分别选择 `singbox` 或 `xray`。

- 每个实例的 `id` 和 `kernel.config_dir` 必须不同，配置目录不可共用。
- 代理监听端口不能冲突，新增端口也不能与机器模式下发的节点端口重叠。启用健康检查时，各实例的健康检查监听也不能冲突；示例均设为 `0`。
- 已有机器的 `token_env` 及其对应的环境变量配置应保持原样，独立实例不需要面板或机器令牌。
- 如果当前使用的是没有 `instances:` 的单实例机器配置，先将整段机器配置移入 `instances:` 下的第一个列表项，再追加独立实例，结构参照上例。

完成修改后执行 `xbctl restart`，再通过 `xbctl log` 查看启动结果。**重启会同时重启所有实例，现有节点连接可能短暂中断。**

## Docker

在源码目录中配置并启动（适用于 Linux，使用宿主机网络）：

```bash
cp examples/node.yml.example config.yml
# 编辑 config.yml，填写面板地址、节点 ID 和内核
export XBOARD_NODE_TOKEN='YOUR_SERVER_TOKEN'
docker compose up -d --build
```

证书等外部文件需要额外挂载。也可用 `docker build -t xboard-node-u:local .` 构建镜像。

## 从源码构建

需要 Go 1.27+、Git 和 Make，以及访问 GitHub 和 Go 模块的网络。

```bash
git clone https://github.com/Toto0912/xboard-node-u.git
cd xboard-node-u
make build REPOSITORY=Toto0912/xboard-node-u
./xboard-node -v
```

构建 Linux amd64 / arm64：

```bash
make build-all VERSION=dev REPOSITORY=Toto0912/xboard-node-u
```

构建会自动下载固定提交并应用内核和传输依赖补丁，生成的 `third_party/` 不提交到 Git。请保留准备步骤及 `go.mod` 的 replace。

[详细构建说明（包括 Windows）](docs/BUILD.md) · [发布流程](docs/RELEASE.md)

## 开发文档

- [sing-box 升级说明](docs-singbox-upgrade.md)
- [Xray 升级说明](docs-xray-upgrade.md)
- [自定义路由](docs-custom-routes.md)
- [自定义出站](docs-custom-outbounds.md)
- [DNS 证书服务商](docs-dns-providers.md)

## 来源与许可证

项目仓库：[Toto0912/xboard-node-u](https://github.com/Toto0912/xboard-node-u)。本分支不代表原项目或两套内核的官方发行。
保留上游 Go 模块路径用于内部导入，安装和在线更新指向本仓库。

原节点项目 README 声明 MPL-2.0；内核及依赖各自保留许可证，包括 sing-box 的 GPLv3-or-later 和名称声明。
分发程序时请保留对应源码、补丁和许可证，参见 [LICENSE](LICENSE) 与 [第三方来源说明](THIRD_PARTY_NOTICES.md)。
