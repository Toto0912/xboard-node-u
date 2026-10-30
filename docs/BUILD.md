# 构建说明

## 固定依赖

- Go 1.27+，Git；Make 为可选工具。
- sing-box v1.14.4，提交 `c005f35300ce76427dc285481ec5695f5b179cd2`。
- Xray v26.10.29，提交 `0afec66085a85191e9dd2a4a6646c7c9a94688d1`。
- Go 模块依赖使用 `go.mod` 和 `go.sum`，内核使用本地 replace 与版本化补丁。

Xray 的规范 Go 伪版本 `v1.260327.1-0.20261029083850-0afec66085a8` 对应上述 10 月提交，不是旧内核。

## Linux / macOS

```sh
make prepare
make build
make test
make build-all VERSION=dev REPOSITORY=owner/repository
```

`make test` 启用 race detector，需要 C 编译器。没有 C 编译器时可运行不带 race 的检查：

```sh
go test -tags 'with_quic with_utls with_wireguard with_clash_api' ./internal/... ./cmd/... ./scripts/...
```

`REPOSITORY` 只决定 xbctl 在线更新地址，不改变 Go 模块路径。未设置的本地构建不默认下载任何仓库的 Release。

## Windows PowerShell

```powershell
go run ./scripts/prepare-singbox.go
go run ./scripts/prepare-xray
go test -tags 'with_quic with_utls with_wireguard with_clash_api' ./internal/... ./cmd/... ./scripts/...
go build -tags 'with_quic with_utls with_wireguard with_clash_api' -o xboard-node.exe ./cmd/xboard-node
go build -o xbctl.exe ./cmd/xbctl
```

交叉编译 Linux：

```powershell
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64' # arm64 同理
go build -trimpath -tags 'with_quic with_utls with_wireguard with_clash_api' -o xboard-node-linux-amd64 ./cmd/xboard-node
go build -trimpath -o xbctl-linux-amd64 ./cmd/xbctl
```

Windows 不执行 Linux 文件；测试结束后可删除这几个进程环境变量，再运行本地 Go 测试。

## 源码归档与内核目录

GitHub 自动生成的 Source code.zip 不包含生成目录，需要先运行两个 prepare 命令。
发布工作流额外提供 `source-with-kernels.tar.gz`，包含两套已应用补丁的内核，排除所有 .git 目录。
从此完整归档构建时，跳过 prepare，直接运行上述 `go test` / `go build` 命令；依赖模块仍需网络或已有缓存。
准备脚本仅接受可核验提交的 Git checkout，不会把缺少 .git 的归档目录当成可信 checkout。

`third_party/` 是生成目录，不要手工改动后直接升级补丁；重新开发内核时须同时更新来源提交、补丁、go.mod 版本和回归测试。

## Docker 与 CI

Dockerfile 在构建阶段准备两套内核，产物为嵌入双内核的单一程序。
本地未启动 Docker 守护进程，镜像执行未在本机验证。GitHub CI 执行 Linux race 测试及 amd64/arm64 构建；只有推送版本标签才创建 Release 和推送镜像。
