# Linux systemd 安装与管理

## 交互安装

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install.sh | sudo bash -s -- --interactive
```

需要 curl、Bash 和 systemd。脚本从终端读取输入，因此可以通过管道执行；不会把后面的脚本内容当成输入。
默认机器模式、sing-box 内核。按提示输入面板地址、机器/节点 ID 和对应密钥。
机器模式用机器令牌，单节点模式用面板通信密钥。输入密钥时终端不显示字符。

安装器自动识别 Linux amd64/arm64，下载最新正式版 xboard-node 和 xbctl，创建配置及 systemd 服务并设为开机启动。
修改配置后用 `sudo xbctl service restart` 重启。

## 无人值守安装

机器模式、sing-box：

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install.sh | sudo bash -s -- --mode machine --panel https://panel.example.com --token 'YOUR_MACHINE_TOKEN' --machine-id 1 --kernel singbox
```

单节点模式、Xray：

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/latest/download/install.sh | sudo bash -s -- --mode node --panel https://panel.example.com --token 'YOUR_SERVER_TOKEN' --node-id 1 --kernel xray
```

以上无人值守示例需先替换面板地址、密钥和 ID。内核可选 `singbox` / `xray`。

## 指定版本

```bash
curl -fsSL https://github.com/Toto0912/xboard-node-u/releases/download/v1.0.0/install.sh | sudo bash -s -- --interactive --version v1.0.0
```

安装脚本与二进制版本保持一致。若只使用预发布版本，请显式指定版本号，latest 指向正式版。

## 离线或本地程序

将同一版本、同一架构的两个程序和 install.sh 放在当前目录：

```bash
sudo bash install.sh --interactive --binary ./xboard-node-linux-amd64 --xbctl-binary ./xbctl-linux-amd64
```

ARM64 替换文件名。离线机器需已具备运行依赖；安装器仍会尝试检查或安装系统依赖。

## 管理

```bash
sudo xbctl status
sudo xbctl list
sudo xbctl service restart
sudo xbctl upgrade
sudo xbctl upgrade --version v1.0.0
sudo journalctl -u xboard-node.service -n 50 --no-pager
```

- 配置：`/etc/xboard-node/config.yml`
- 凭据：`/etc/xboard-node/credentials.env`，安装器以受限权限保存
- 程序：`/usr/local/bin/xboard-node`、`/usr/local/bin/xbctl`
- 安装脚本副本：从本地脚本文件安装时保存在 `/etc/xboard-node/install.sh`；通过管道安装时不保存副本
- 服务：`xboard-node.service`

`xboard-node -v` 显示程序版本与两套内核版本。安装器会备份已有安装，失败时尝试回滚。
无面板 standalone 使用本地配置启动，见 [示例](../examples/standalone.yml.example)，不是交互安装器的面板接入模式。

## 自己的 fork

本仓库安装脚本默认从 Toto0912/xboard-node-u 下载，不会回落到原作者仓库。
在自己的 fork 中使用时，替换下载脚本的 URL，并设置：

```bash
curl -fsSL https://github.com/OWNER/REPOSITORY/releases/latest/download/install.sh | sudo env XBOARD_NODE_REPOSITORY=OWNER/REPOSITORY bash -s -- --interactive
```

GitHub Actions 构建的 xbctl 自动嵌入构建仓库；本地构建使用 `make build REPOSITORY=OWNER/REPOSITORY`。
