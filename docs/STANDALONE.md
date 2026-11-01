# 无面板独立运行（裸核运行）

独立模式通过 `standalone` 读取本地节点和用户，不需要面板地址、通信密钥或机器 ID，不向面板同步用户或上报流量。代理仍由 xboard-node-u 内嵌的内核运行，配置格式为本项目的 YAML。

## 单独运行一个节点

以下步骤适用于已经安装 xboard-node-u 和后台服务的服务器；安装向导本身只提供机器模式和面板单节点模式。

选择一个完整示例，将内容复制到 `/etc/xboard-node/config.yml`，替换占位参数后保存：

| 协议 | 配置示例 | 需要修改 | 放行端口 |
| --- | --- | --- | --- |
| SS | [完整配置](../examples/standalone.yml.example) | 密码、监听端口 | 示例为 8388 TCP / UDP |
| VLESS + Reality | [完整配置](../examples/standalone-vless-reality.yml.example) | UUID、Reality 私钥、short ID、目标域名及端口 | 示例为 443 TCP |
| HY2 | [完整配置](../examples/standalone-hy2.yml.example) | 密码、域名、证书路径及端口 | 示例为 8443 UDP |

三个示例均使用 sing-box。配置只保留所选示例，不要把三个完整示例直接拼接在文件最外层。示例监听所有 IPv4 接口，客户端填写服务器公网 IP 或域名。

以 root 执行：

```sh
xbctl restart
xbctl log
```

如果只需要前台运行已有程序，可直接指定配置文件：

```sh
xboard-node -c /path/to/config.yml
```

### 客户端参数

- **SS**：服务器地址、端口与服务端对应；加密使用 `chacha20-ietf-poly1305`，密码对应 `standalone.users[].uuid`。
- **VLESS + Reality**：使用真实 UUID 和自己生成的 Reality X25519 密钥对；私钥填入服务端 `private_key`，公钥填入客户端。客户端使用相同的 UUID、SNI（`server_name`）和 short ID，传输为 TCP，安全为 Reality，flow 为 `xtls-rprx-vision`。示例域名必须替换为实际可用的 Reality 目标，客户端指纹可设为 `chrome`。
- **HY2**：认证密码对应 `standalone.users[].uuid`。提前准备与实际域名匹配的证书及私钥，修改 `cert_file`、`key_file`；客户端 SNI 使用该域名，并正常验证证书。`protocol: hysteria` 加 `version: 2` 表示 HY2。

## 与现有机器模式共存

如果配置已经以 `instances:` 开头，保留原机器实例，在**同一个列表**中追加独立实例；不要再添加第二个 `instances:`。下面展示机器模式与 SS 共存的完整结构，面板及凭据均为占位示例：

```yaml
instances:
  - id: panel-machine
    panel:
      url: https://panel.example.com
    machine:
      machine_id: 1
      token_env: XBOARD_NODE_MACHINE_TOKEN
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/panel-machine
    log:
      level: info
      output: stdout
    health_port: 0

  - id: standalone-ss
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/standalone-ss
      log_level: warn
    standalone:
      enabled: true
      node:
        protocol: shadowsocks
        listen_ip: "0.0.0.0"
        server_port: 8388
        cipher: chacha20-ietf-poly1305
      users:
        - id: 1
          uuid: "REPLACE_WITH_YOUR_SS_PASSWORD"
    log:
      level: info
      output: stdout
    health_port: 0
```

保留自己原有的机器实例，包括 ID、内核、`token_env` 和对应凭据文件；只追加第二项。VLESS + Reality 或 HY2 也可照此追加，将相应完整示例缩进到一个新的 `- id:` 下，HY2 的 `cert` 同样放在该实例内。

每个实例的 `id`、`kernel.config_dir` 和代理监听端口必须不同，且不能与面板下发的节点冲突。示例中的 `health_port: 0` 表示关闭健康检查监听，与代理端口无关。

若原来没有 `instances:`，先把原有机器配置整体缩进到第一个实例，再追加独立实例。保存后执行 `xbctl restart`；该操作会重启所有实例，已有连接会短暂中断。
