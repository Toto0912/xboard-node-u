# Alpine 与统一 xbctl 本地验证

验证日期：2026-10-30（Asia/Shanghai）。本次只修改安装、管理与发布流程，节点入口及两套内核源码未改。

## 本地环境

- Windows 本机运行 Docker，隔离 Alpine **3.22.7 / amd64** 容器，OpenRC **0.62.6**。
- 安装器实际使用 `/bin/sh`；独立最小环境确认无 Bash、Python 仍可安装与管理。
- 节点程序使用已发布的 v1.0.0，CLI 使用本次源码编译的 Linux amd64 `xbctl`。
- CLI 另已交叉编译 Linux arm64；ARM64 未进行运行测试。
- 源码、程序与校验清单以只读方式挂载，所有安装与服务操作仅发生在临时容器内。
- 没有操作 VPS，也没有向真实 Xboard 面板发送测试请求。

## 已通过

Go CLI 与配置测试、`go vet ./cmd/xbctl` 均通过。CLI 测试既在 Windows 执行，也编译为 Linux 测试程序在 Alpine 中执行，包含：

- systemd / OpenRC 自动选择及 systemd 优先级。
- 两套服务管理器的启动、停止、重启、启用、禁用命令分派和错误传递。
- OpenRC 日志参数与真实节点子进程检查。
- 安装元信息生成不改写配置；重写绑定保留旧配置中的内嵌令牌。
- 发布仓库选择、覆盖和非法地址拒绝。

Alpine 主集成测试 **21 组**通过，覆盖：

- 真实终端交互、隐藏令牌、引号/反斜杠/美元符按字面保存、配置权限。
- 节点和 xbctl 成对 SHA256 校验；支持 Windows CRLF 校验文件。
- 拒绝不支持 OpenRC 的旧 xbctl，且不停止现有服务。
- 从已运行的独立 Alpine 安装迁移到 xbctl，保留原配置并移除 xb-alpine。
- `xbctl status/list/service status/service logs/enable/disable`。
- 机器模式和单节点模式，各自使用 sing-box / Xray 同步配置与用户，通过 VLESS/TCP 实际转发返回 HTTP 200。
- 杀死真实节点进程后，OpenRC 拉起新进程并恢复代理。
- xbctl 绑定添加/移除、绑定后重启、移除最后绑定后停止服务。
- xbctl 升级保留配置与凭据，并更新安装元信息。
- 注入无法启动的新程序，恢复旧节点、CLI、配置与服务状态；原服务停止时，失败后仍保持停止。
- OpenRC default 运行级重新进入后启动服务（容器启动流程模拟）。
- 取消卸载、确认卸载保留配置，以及显式 `--purge` 清理配置。

旧看门狗迁移测试另有 **5 组**通过：真实活动进程拦截、失败回滚、残留文件自动清理、旧 PID 复用不误杀、只清理匹配的自启行并保留其他任务，以及迁移后再次升级。

最小环境测试通过：只预装 OpenRC 的 Alpine 容器，自动补齐安装依赖，启动正式节点与本次 CLI，经 xbctl 查看状态和卸载；没有 Bash、Python、sudo 或 systemd。

原 systemd 安装器 `install.sh` 未修改；其语法、管道交互向导、两种模式、隐藏输入、无效输入重试和仓库覆盖回归通过。

## 验证边界

- 模拟面板、代理目标均位于容器回环地址，不测试用户公网线路、CDN 或真实通信密钥。
- 回环代理放行规则只存在于测试夹具中，安装器生成的生产配置不包含这些规则。
- 本次未重新执行全部内核协议/并发测试，也未重新构建完整节点内核；本次源码修改集中在 CLI 和安装器。
- systemd 没有在本次重新部署实机；其安装向导和管理命令分派进行了回归。
- 没有重启真实 LXC，也没有 ARM64 实机运行测试。
- 新 Release 的远程下载附件在发布后才存在；本地及 CI 使用同一份清单中的待发布节点/CLI 进行安装测试。

## 自动化复现

CI 的 Alpine job 在 Linux 构建完成后，使用同一工作流生成的 amd64 节点与 CLI，建立测试校验清单。
随后分别运行 `scripts/alpine/bootstrap.sh`、`bootstrap.sh legacy.py` 和干净 Alpine 容器中的 `minimal.sh`。

所有测试必须在临时容器内运行，**不要直接在生产主机执行测试脚本**。
Alpine 测试通过后，Release 才会打包 `install-alpine.sh` 并将其加入 SHA256 清单。
