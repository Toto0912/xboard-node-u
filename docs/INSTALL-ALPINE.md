# Alpine Linux / OpenRC 安装

使用独立的 `install-alpine.sh`；`install.sh` 继续管理 systemd 环境。
支持 amd64 / arm64，机器模式 / 单节点模式，以及 sing-box / Xray。

## 一键安装

在 Alpine LXC/VPS 内以 root 执行：

```sh
apk add --no-cache curl ca-certificates && curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install-alpine.sh -o /tmp/install-alpine.sh && sh /tmp/install-alpine.sh --interactive
```

默认机器模式、sing-box，令牌输入不回显。填写面板网站根地址，不包含管理员入口路径。
机器模式使用机器令牌；单节点使用面板通信密钥。

脚本使用 Alpine 的 `/bin/sh`，不需要 Bash、Python、Docker、sudo 或 systemd。
需要系统已有正在运行的 OpenRC。脚本安装 curl、ca-certificates、logrotate 等所需依赖。
程序与 `xbctl` 从同一正式 Release 下载并校验 SHA256；旧版不支持 OpenRC 的 `xbctl` 会在替换文件前被拒绝。
此方式从包含 Alpine 安装器与新版 `xbctl` 的正式 Release 开始提供，不能用于 v1.0.0。

## 统一管理

```sh
xbctl status
xbctl list
xbctl service backend       # 显示 openrc 或 systemd
xbctl service restart
xbctl service logs          # 跟随日志，Ctrl+C 退出
xbctl service logs -n 50 --no-pager
xbctl service enable
xbctl service disable
xbctl upgrade
```

`xbctl start/stop/restart`、`xbctl logs` 等快捷命令同样可用。
`xbctl bind` 系列命令在修改绑定后通过 OpenRC 重启/停止服务，调用方式与 systemd 相同。
`xbctl upgrade` 调用已保存的 Alpine 安装器，成对更新节点和 CLI，保留配置与凭据；失败时尝试回滚。

## 配置和迁移

- 配置：`/etc/xboard-node/config.yml`，权限 600。
- 凭据：兼容已有 `/etc/xboard-node/credentials.env`；按字面导出，不作为 shell 代码执行。
- OpenRC 服务：`/etc/init.d/xboard-node`，加入 default 运行级。
- 安装器副本：`/etc/xboard-node/install-alpine.sh`。
- 备份：`/etc/xboard-node/backups/alpine-*`。
- 日志：`/var/log/xboard-node/node.log`、`error.log`。

已通过早期独立脚本安装、使用 `xb-alpine` 管理的用户，重新运行本安装器即可迁移为 `xbctl`；配置与凭据默认按字节保留。
旧管理工具在备份后移除。旧看门狗已经停止但留下脚本/PID 时，会自动备份清理相应残留和有明确旧标记的自启行；其他 cron/启动任务保留。
如果旧进程实际还在运行，脚本会显示 PID 并停止安装，不会根据旧 PID 文件误杀其他进程。

重新填写绑定和内核：

```sh
sh /etc/xboard-node/install-alpine.sh install --reconfigure
```

默认不开额外健康端口。启动检查确认真实节点进程持续存活；面板连接、节点下发是否正常仍请看日志。
日志轮转依赖系统 crond；未运行时安装器会提示，不修改其他 cron 任务。
LXC 使用共享公网 IP 时，仍需由宿主机映射对应 TCP/UDP 节点端口。

## 无人值守 / 离线

令牌放在仅 root 可读文件的首行，避免将密钥直接写入命令历史：

```sh
sh install-alpine.sh --mode machine --kernel singbox \
  --panel https://panel.example.com --id 1 --token-file /root/machine-token
```

单节点将 `--mode` 改为 `node`，`--id` 填节点 ID，令牌文件中放面板通信密钥。

离线安装需准备同一版本、相同架构的两个程序和官方清单，并已具备系统依赖：

```sh
sh install-alpine.sh --binary ./xboard-node-linux-amd64 \
  --xbctl-binary ./xbctl-linux-amd64 --checksums ./SHA256SUMS.txt --version v1.0.1
```

也可在升级时使用这些参数。自有 fork 可设置 `XBOARD_NODE_REPOSITORY=OWNER/REPOSITORY`。

## 卸载

```sh
xbctl uninstall --yes          # 保留配置、凭据、备份、运行数据和日志
xbctl uninstall --purge --yes  # 同时删除 /etc/xboard-node
```

本地验证与限制见 [Alpine 验证记录](ALPINE-VALIDATION.md)。
