# xboard-node-u

项目仓库：[Toto0912/xboard-node-u](https://github.com/Toto0912/xboard-node-u)。

基于 [cedar2025/Xboard-Node](https://github.com/cedar2025/Xboard-Node) 的双内核节点后端，连接 [Xboard 面板](https://github.com/cedar2025/Xboard)，或脱离面板独立运行。

本分支升级到以下固定版本，保留节点管理、用户热更新、限速和流量上报所需的兼容补丁：

| 内核 | 上游版本 | 本项目补丁 |
| --- | --- | --- |
| sing-box | 1.14.4 | `patches/sing-box-1.14.4.patch` |
| Xray-core | 26.10.29 | `patches/xray-core-26.10.29.patch` |

另对同版本依赖 sing-vmess、sing-quic 应用认证表并发补丁，见 `patches/sing-vmess-user-snapshot.patch` 和 `patches/sing-quic-tuic-users.patch`；构建时自动准备，不需要手工修改模块缓存。

运行 `xboard-node -v` 可查看节点程序和两套内核版本。内核嵌入程序，无需另外安装 sing-box 或 Xray 可执行文件。

## 功能与部署模式

- 机器模式：自动发现和管理面板绑定到机器的节点。
- 单节点模式：通过节点 ID 和面板通信密钥接入面板。
- standalone 模式：本地配置协议和用户，不连接面板。
- 支持多实例、多面板、WebSocket 推送及 REST 轮询。
- 保留原有协议接入、用户限速、设备限制和流量统计功能；具体协议和字段取决于所选内核及面板支持情况。

这是经测试的升级分支，尚未完成长期负载验证。内核升级不等于面板已经支持全部新增协议参数，详见 [验证范围](docs/VALIDATION.md)。

## 从源码构建

需要 Go 1.27 或更新版本、Git，以及访问 GitHub 和 Go 模块的网络。Linux/macOS 安装 Make 后可以运行：

```sh
git clone https://github.com/Toto0912/xboard-node-u.git
cd xboard-node-u
make build
./xboard-node -v
```

Linux amd64 / arm64 构建：

```sh
make build-all VERSION=dev
```

准备程序会下载固定提交、核验版本并应用补丁，生成的 `third_party/` 不提交到仓库。请勿直接删除 `go.mod` 的 replace 或省略准备步骤。

[详细构建说明（包括 Windows）](docs/BUILD.md)

## Linux systemd 安装

从自己的 GitHub Release 下载同一版本、同一架构的 `xboard-node-linux-amd64` 与 `xbctl-linux-amd64`，以及此项目的 `install.sh`；也可以使用本地构建结果。

单节点模式（默认 sing-box）：

```sh
sudo bash install.sh --mode node \
  --panel https://panel.example.com --token 'YOUR_SERVER_TOKEN' --node-id 1 \
  --kernel singbox \
  --binary ./xboard-node-linux-amd64 --xbctl-binary ./xbctl-linux-amd64
```

机器模式（示例明确使用 Xray）：

```sh
sudo bash install.sh --mode machine \
  --panel https://panel.example.com --token 'YOUR_MACHINE_TOKEN' --machine-id 1 \
  --kernel xray \
  --binary ./xboard-node-linux-amd64 --xbctl-binary ./xbctl-linux-amd64
```

ARM64 请替换文件名。安装器会生成 `/etc/xboard-node/config.yml`、受限权限的凭据文件和 systemd 服务。
两种模式的 token 含义不同：单节点使用面板通信密钥，机器模式使用对应机器的 token。

安装器需要下载二进制时，必须显式设置 `XBOARD_NODE_REPOSITORY=Toto0912/xboard-node-u`。GitHub Actions 构建的 `xbctl` 会自动嵌入当前仓库；本地构建可用 `make build REPOSITORY=Toto0912/xboard-node-u`。本分支不会默认从原作者 Release 下载旧程序。推送 main 只构建测试附件，正式版本附件需要发布标签后生成。详见 [发布与更新](docs/RELEASE.md)。

## 配置

- [单节点示例](examples/node.yml.example)
- [机器模式示例](examples/machine.yml.example)
- [无面板示例](examples/standalone.yml.example)
- [多实例示例](config.yml.example)

选择内核时显式设置 `kernel.type: singbox` 或 `kernel.type: xray`；机器模式也使用这个设置。协议兼容性决定了部分节点可能选择其他内核，请以启动日志为准。

```sh
cp examples/node.yml.example config.yml
export XBOARD_NODE_TOKEN='YOUR_SERVER_TOKEN'
./xboard-node -c config.yml
```

## Docker

从本项目构建镜像，不依赖原作者镜像：

```sh
cp examples/node.yml.example config.yml
export XBOARD_NODE_TOKEN='YOUR_SERVER_TOKEN'
docker compose up -d --build
```

Compose 使用宿主机网络，适用于 Linux。替换配置中的面板地址、节点 ID 和内核。证书等外部文件需要额外挂载。
也可使用 `docker build -t xboard-node:local .` 构建镜像。

## 管理和开发

```sh
xbctl list
xbctl status
xbctl service restart
```

- [sing-box 升级说明](docs-singbox-upgrade.md)
- [Xray 升级说明](docs-xray-upgrade.md)
- [自定义路由](docs-custom-routes.md)
- [自定义出站](docs-custom-outbounds.md)
- [DNS 证书服务商](docs-dns-providers.md)
- [变更记录](CHANGELOG.md)

## 来源与许可证

保留上游模块路径 `github.com/cedar2025/xboard-node`，不影响 fork 后编译；该路径用于内部 Go 导入，不是下载更新的仓库地址。
本分支不代表原作者或两套内核的官方发行。

原节点项目 README 声明 MPL-2.0；本目录补齐该许可证文本。两套内核及其他依赖各自保留许可证，尤其 sing-box 的 GPLv3-or-later 及附加名称声明。分发包含这些组件的程序时，应同时提供对应源代码、补丁和许可证，不能把完整双内核程序简单描述为只有 MPL-2.0。
参见 [LICENSE](LICENSE) 与 [第三方来源和许可证](THIRD_PARTY_NOTICES.md)。
