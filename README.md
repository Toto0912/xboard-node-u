# xboard-node-u

基于 [Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，支持 Xboard 面板的机器模式、单节点模式，以及无面板独立运行。

| 内核 | 当前版本 |
| --- | --- |
| sing-box | 1.14.4（含 Xboard 兼容补丁） |
| Xray-core | 26.10.29（含 Xboard 兼容补丁） |

两套内核已嵌入程序，无需另装内核。[下载正式版](https://github.com/Toto0912/xboard-node-u/releases/latest) · [变更记录](CHANGELOG.md)

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

## 功能与配置

- 自动同步面板节点配置和用户，支持 WebSocket 推送及 REST 轮询。
- 支持多实例、多面板、用户热更新、限速、设备限制和流量统计。
- 协议和参数支持取决于面板与所选内核，详见 [验证范围](docs/VALIDATION.md)。

配置示例：[单节点](examples/node.yml.example) · [机器模式](examples/machine.yml.example) · [无面板独立运行](examples/standalone.yml.example) · [多实例](config.yml.example)

选择内核时设置 `kernel.type: singbox` 或 `kernel.type: xray`，机器模式也使用这一设置。协议兼容性可能触发内核选择调整，请以启动日志为准。

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
