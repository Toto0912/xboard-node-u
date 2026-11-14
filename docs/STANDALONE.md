# 无面板独立运行（裸核运行）

独立模式通过 `standalone` 读取本地节点和用户，不需要面板地址、通信密钥或机器 ID，不向面板同步用户或上报流量。代理仍由 xboard-node-u 内嵌的内核运行，配置格式为本项目的 YAML。

需要在服务端自定义 DNS、出站或高级路由时，可在该独立实例的 `kernel.custom_config` 引用本地文件，详见 [自定义内核配置](CUSTOM-CONFIG.md)。此功能也适用于机器模式和面板单节点模式。

## 单独运行一个节点

以下步骤适用于已经安装 xboard-node-u 和后台服务的服务器；安装向导本身只提供机器模式和面板单节点模式。

选择一个完整示例，将内容复制到 `/etc/xboard-node/config.yml`，替换占位参数后保存：

| 协议 | 配置示例 | 需要修改 | 放行端口 |
| --- | --- | --- | --- |
| SS | [完整配置](../examples/standalone.yml.example) | 密码、监听端口 | 示例为 8388 TCP / UDP |
| VLESS + Reality | [完整配置](../examples/standalone-vless-reality.yml.example) | UUID、Reality 私钥、short ID、目标域名及端口 | 示例为 443 TCP |
| HY2 | [完整配置](../examples/standalone-hy2.yml.example) | 密码、域名、证书路径及端口 | 示例为 8443 UDP |
| Xray Reality 原生回落 | [完整配置](../examples/standalone-xray-reality-fallback.yml.example) | UUID、Reality 私钥、Short ID、原目标和明文回落后端 | 示例为 443 TCP |
| VLESS + TLS 回落 | [完整配置](../examples/standalone-vless-fallback.yml.example) | UUID、域名、证书路径及本地网站端口 | 示例为 443 TCP |

SS、Reality 和 HY2 示例使用 sing-box，TLS 回落和 Reality 原生回落示例使用 Xray。配置只保留所选示例，不要把多个完整示例直接拼接在文件最外层。示例监听所有 IPv4 接口，客户端填写服务器公网 IP 或域名。

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

## Xray fallback 回落

**直接在节点端正在使用的主配置文件中添加回落配置，不需要另建 `.yml` 文件，也不需要通过 `kernel.custom_config` 引入。** 如果你的主配置文件是 `/etc/xboard/config.yml`，就在这个文件中编辑。本项目安装脚本默认使用 `/etc/xboard-node/config.yml`；实际编辑哪个文件，以服务启动命令的 `-c` 参数为准，不要把配置写进另一个未被加载的文件。

在主配置文件中找到独立节点的 `standalone:`，再找到它下面的 `node:`，将 `fallbacks:` 添加到 `node:` 内，**与 `tls:`、`tls_settings:` 同级**。文档中的 `standalone.node.fallbacks` 只是这三层 YAML 的简写，不是另一个文件，也不是可以直接写在最外层的字段。

本节适用于无面板独立模式、`kernel.type: xray`、VLESS / Trojan、TCP（`tcp` / `raw`）。从 **v1.1.2** 起，普通 TLS（`tls: 1`）与 Reality（`tls: 2`）统一使用 **`standalone.node.fallbacks` 列表**，按 Xray 原生协议层回落处理；独立模式不使用 `xray_fallback` 字段。

### Reality：原目标与回落列表分别配置

**`fallbacks` 不覆盖 `tls_settings.dest`，也不替换原 SNI、私钥、Short ID 或 Flow。** 两条路径分别是：

| 连接情况 | 去向 |
| --- | --- |
| 普通浏览器或未通过 Reality 鉴权 | 原 `tls_settings.dest`（HTTPS / TLS 1.3 目标） |
| 已通过 Reality 握手，后续 VLESS / Trojan 协议首包或认证无效 | `fallbacks` 选择的后端；收到的是已解密数据，通常使用明文 HTTP 服务 |
| Reality 与代理协议认证均通过 | 正常代理，按路由处理 |

下面是**主配置文件的完整示例**。HTTPS 伪装目标和明文回落后端是两项独立配置，不能混用：

```yaml
kernel:
  type: xray
  config_dir: /etc/xboard-node/standalone-xray-reality-fallback
  log_level: warn
standalone:
  enabled: true
  node:
    protocol: vless
    listen_ip: "0.0.0.0"
    server_port: 443
    network: tcp
    tls: 2
    flow: xtls-rprx-vision
    tls_settings:
      private_key: "REPLACE_WITH_YOUR_REALITY_PRIVATE_KEY"
      short_id: "0123456789abcdef"
      server_name: "site.example.com"
      dest: "site.example.com:443"  # 原 HTTPS 伪装目标，fallbacks 不会覆盖它
    fallbacks:                    # 与 tls_settings 同级；这里接收解密后的数据
      - dest: "127.0.0.1:8080"
      - path: /special
        dest: "127.0.0.1:8081"
  users:
    - id: 1
      uuid: "00000000-0000-4000-8000-000000000001"
log:
  level: info
  output: stdout
health_port: 0
```

完整文件见 [Xray Reality 原生回落示例](../examples/standalone-xray-reality-fallback.yml.example)。如果要“Reality 偷自己”，应把 **`tls_settings.dest`** 改成自己网站的 HTTPS 地址，例如 `127.0.0.1:8443`；该网站须支持 TLS 1.3，证书匹配原 `server_name`。不要通过 `fallbacks` 覆盖目标。

Reality 不使用普通 TLS 的 `tls_settings.alpn` 来强制协商协议。当前嵌入的 Xray 26.11.9 在已认证 Reality 连接中协商的 ALPN 为空，非空 `alpn` 规则可能不会命中；通常省略该条件，并保留一条默认规则。规则按实际连接状态匹配，不会将它强制改成 HTTP/1.1。

### 普通 TLS：列表写法不变

完整示例见 [VLESS + TLS 回落](../examples/standalone-vless-fallback.yml.example)。提前准备与域名匹配的证书，并在 `127.0.0.1:8080` 运行明文 HTTP 网站。浏览器访问代理的 HTTPS 端口时，非代理流量会回落到该网站；合法代理用户继续走原代理协议。

下面是**单独运行一个 VLESS + TLS 回落节点时，主配置文件的完整结构**。替换域名、证书路径、UUID 和端口；已经有独立节点配置时，按这个层级补充 `fallbacks`，不要在文件末尾重复粘贴一份 `standalone:`。

```yaml
kernel:
  type: xray
  config_dir: /etc/xboard-node/standalone-vless-fallback
  log_level: warn

cert:
  cert_mode: file
  cert_file: /etc/xboard-node/certs/fullchain.pem
  key_file: /etc/xboard-node/certs/privkey.pem

standalone:
  enabled: true
  node:
    protocol: vless
    listen_ip: "0.0.0.0"
    server_port: 443
    network: tcp
    tls: 1
    flow: xtls-rprx-vision
    tls_settings:
      server_name: "proxy.example.com"
      alpn: [http/1.1]

    # 就在这里添加，与 tls_settings 同级
    fallbacks:
      - dest: "127.0.0.1:8080"
        xver: 0

  users:
    - id: 1
      uuid: "00000000-0000-4000-8000-000000000001"

log:
  level: info
  output: stdout
health_port: 0
```

**如果主配置文件已经使用 `instances:` 管理多个节点**，就在对应的独立实例下添加。下面只展示回落配置的位置，原有 `kernel`、`cert`、用户及其他实例均保留：

```yaml
instances:
  # 原有面板节点或机器实例保留在这个列表中

  - id: standalone-vless-fallback
    # 此实例原有的 kernel、cert 等配置保留
    standalone:
      enabled: true
      node:
        # 此实例原有的 protocol、端口、tls: 1 或 2 等配置保留
        tls_settings:
          server_name: "proxy.example.com"
          alpn: [http/1.1]
        fallbacks:
          - dest: "127.0.0.1:8080"
            xver: 0
      # 原有 users 保留在这里，与 node 同级
```

多条回落规则也写在同一个 `fallbacks:` 列表中，可按需添加 `name`、`alpn` 或 `path` 匹配条件。保存主配置文件后执行 `xbctl restart`，再用 `xbctl log` 查看启动情况；重启会短暂中断该服务管理的所有实例连接。

| 字段 | 含义 |
| --- | --- |
| `dest` | 必填；支持端口号、`IP或域名:端口`、`[IPv6]:端口` 或 Unix socket 路径 |
| `name` | 可选，按客户端 TLS SNI 匹配 |
| `alpn` | 可选，按实际协商的 ALPN 匹配；该值须包含在 `tls_settings.alpn` 中 |
| `path` | 可选，按首包 HTTP 路径匹配，非空时以 `/` 开头；不用于 HTTP/2 路径匹配 |
| `xver` | 默认为 `0`；`1` / `2` 向后端发送对应版本的 PROXY protocol，后端也必须配置接收 |

普通 TLS 省略 `tls_settings.alpn` 时，回落入站默认使用 `[http/1.1]`。需要 HTTP/2 回落时，显式设置 `[h2, http/1.1]` 并提供能处理 h2c 的对应后端。尽量保留一条不指定 `name`、`alpn`、`path` 的默认规则。

只写端口号时 Xray 会连接 `localhost`，可能解析到 IPv6；后端只监听 IPv4 时，请像示例一样明确填写 `127.0.0.1:端口`。回落目标不能指回代理自身的监听端口，也不要把网站后端开放到公网。

此入口在 TLS 和 Reality 下均不能与 VLESS 的额外 `decryption` 加密一起使用。不支持的内核、协议、传输、安全性组合及无效参数会在加载配置时明确报错。`kernel.custom_config` 仍不合并自定义入站；请使用上述专用配置。

### 面板模式与版本兼容

面板机器模式和单节点模式在 xboard-u 的 Fallback 输入框填写单目标，节点端转换为一条原生 `fallbacks` 规则，无需重复填写本地列表。面板和节点端须一并升级到 **v1.1.2**，sing-box 继续忽略面板下发的回落字段。

旧版曾将面板 Reality Fallback 用于覆盖目标站；v1.1.2 有意改为原生协议层语义。升级前检查这类节点：HTTPS 伪装目标放在 Reality 设置中，回落目标改为接收解密数据的服务；配置数据不会自动删除或替换。尚未正式发布的单目标独立写法 `xray_fallback` 已取消，请改为上述 `fallbacks` 列表。

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

## HY2 带宽与伪装（v2.2.0）

可在本文件对应实例的 `standalone.node.hy2_settings` 配置自动带宽策略与 Masquerade，无需另建文件。独立配置的带宽是服务端视角。完整写法及两内核示例见 [HY2 配置说明](HYSTERIA2.md)。
