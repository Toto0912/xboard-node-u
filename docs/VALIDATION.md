# 验证范围

最后整理：2026-10-30。以下区分已有功能测试与尚未覆盖的情况，不包含服务器地址或测试凭据。

## 已完成

- Windows amd64、Go 1.27.3 本地协议与单元测试。
- Linux amd64、arm64 交叉编译；Linux amd64 测试 VPS 实际运行。
- sing-box：VMess、VLESS、Shadowsocks、SOCKS、HTTP、Trojan、AnyTLS、Hysteria2、TUIC 的真实回环连接、用户更新及流量统计。
- Xray：VMess、VLESS、Trojan TLS、传统 SS、SS2022、VLESS WS/gRPC/XHTTP、Mux、XUDP 和 Hysteria2 回环代理测试。
- 用户新增/移除、既有连接上的动态限速、同端口重载、端口冲突回滚、累计统计保留及机器混合内核 WS 配置解析。
- VPS 机器模式、独立节点连接面板、无面板 standalone 三种模式的真实 HTTPS 请求。
- Xray 下原 Hysteria2 节点的公网代理请求；用户进一步确认公网节点访问正常。
- 开发、编译均在本地完成，VPS 仅运行短时功能验证。

## 本次项目整理

将经过以上验证的代码合并为独立源码目录；补齐 README、示例、来源声明和发布工作流。
在线更新改为指向本分支自己的发布仓库，并增加仓库选择回归测试。
整理后已在独立临时目录重新下载固定版本的两套内核、应用补丁并通过全项目测试；
Linux amd64/arm64 的 xboard-node 和 xbctl 均编译成功，GitHub 工作流通过 actionlint 静态校验。
对照已知测试凭据和服务器标识的本地扫描未发现泄漏。

## 边界

- REALITY/Vision、VLESS Encryption 新参数、部分少用协议、长时间负载与多机器规模尚未完整验证。
- 当前测试面板关闭 WebSocket，远程联调使用 REST；WS 修复已有本地回归，未重新开启面板做远程复测。
- 配置重载可能中断连接；跨进程重启的计费连续性不包含在本次新增验证中。
- Windows 本机缺少 C 编译器，未运行 race；Linux race 测试由 GitHub CI 执行。
- Docker 工作流和发布上传需在实际 GitHub 仓库运行后才能确认，不以本地交叉编译代替。
- Xray 上游移除 TLS allowInsecure，并调整 Freedom 对私有地址的默认行为；原生自定义配置需要迁移，详见升级说明。
