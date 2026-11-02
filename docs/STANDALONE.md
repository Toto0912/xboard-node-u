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
| Xray Reality 回落 | [完整配置](../examples/standalone-xray-reality-fallback.yml.example) | UUID、Reality 私钥、short ID、SNI 及本地 HTTPS 网站端口 | 示例为 443 TCP |
| VLESS + TLS 回落 | [完整配置](../examples/standalone-vless-fallback.yml.example) | UUID、域名、证书路径及本地网站端口 | 示例为 443 TCP |

SS、Reality 和 HY2 示例使用 sing-box，TLS 回落和 Xray Reality 回落示例使用 Xray。配置只保留所选示例，不要把多个完整示例直接拼接在文件最外层。示例监听所有 IPv4 接口，客户端填写服务器公网 IP 或域名。

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

### 单目标回落：与面板一致（v1.1.1 起）

在主配置文件中找到独立节点的 `standalone:`，再找到它下面的 `node:`，添加 **`xray_fallback: "目标地址:端口"`，与 `tls:`、`tls_settings:` 同级**。不要写在 `tls_settings:` 里面，也不要直接追加到整个文件最外层。

适用于 Xray、VLESS / Trojan、TCP/RAW；行为与 xboard-u 面板 Fallback 输入框一致：

| 安全性 / 内核 | `xray_fallback` 的作用 |
| --- | --- |
| Xray + 普通 TLS（`tls: 1`） | 生成普通回落，连接 TLS 解密后的明文后端，使用 HTTP/1.1 |
| Xray + Reality（`tls: 2`） | **覆盖 `tls_settings.dest`**，保留 `server_name`（SNI）、私钥、Short ID 和 Flow；目标应是支持 TLS 1.3、证书匹配原 SNI 的 HTTPS 网站 |
| sing-box | 忽略这个字段，继续使用原 `tls_settings.dest` |

目标支持 `域名:端口`、`IP:端口`、`[IPv6]:端口`，端口范围 1–65535。不填写、格式无效或协议组合不适用时忽略，沿用原配置。普通 TLS 下的 VLESS 额外 `decryption` 不适用此回落。

例如 Xray 监听 443，本机 Nginx HTTPS 网站监听 `127.0.0.1:8443`，网站证书对应 `site.example.com`。下面是**主配置文件的完整结构**，替换 UUID、Reality 私钥和网站域名后使用。浏览器访问节点 443 会转发到本机网站，合法 Reality 用户继续使用代理。

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
    xray_fallback: "127.0.0.1:8443"  # 就在这里，与 tls_settings 同级
    tls_settings:
      private_key: "REPLACE_WITH_YOUR_REALITY_PRIVATE_KEY"
      short_id: "0123456789abcdef"
      server_name: "site.example.com"  # 保留此 SNI，不改成 127.0.0.1
      dest: "site.example.com:443"     # Xray 中被上面的有效 xray_fallback 覆盖
  users:
    - id: 1
      uuid: "00000000-0000-4000-8000-000000000001"
log:
  level: info
  output: stdout
health_port: 0
```

完整示例：[Xray Reality 回落](../examples/standalone-xray-reality-fallback.yml.example)。已有 Reality 独立节点时，只需在该节点的 `node:` 中增加 `xray_fallback`，内核设为 `xray`，其余原有参数保留。若使用 `instances:`，添加位置是**对应实例的 `standalone:` → `node:` 内**，不要在根层新建另一个 `standalone:`。保存后执行 `xbctl restart`，再用 `xbctl log` 确认启动；重启会短暂中断该服务管理的所有实例连接。

**从旧写法迁移：**若 Reality 节点写了 `fallbacks: [{dest: "127.0.0.1:8443"}]`，请删除整个 `fallbacks` 列表，改为同级的 `xray_fallback: "127.0.0.1:8443"`。Reality 是目标站转发，不支持将 SNI、ALPN、路径匹配或 PROXY protocol 的普通回落列表直接套用。没有填写新字段时，原 `tls_settings.dest` 写法继续有效。

面板单节点和机器模式仍在面板的 Fallback 输入框填写，无需重复编辑节点端配置；见 [面板节点 Fallback](二次开发功能说明.md#面板节点-fallback)。

### 原生多规则回落：仅普通 TLS

需要按 SNI、ALPN、HTTP 路径匹配时，在同一个 `node:` 内使用原有 `fallbacks:` 列表，与 `tls:`、`tls_settings:` 同级。仅支持独立模式、Xray、VLESS / Trojan、TCP/RAW、普通 TLS（`tls: 1`）。**不要与有效的 `xray_fallback` 同时填写；两者同时配置会明确报错。**

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
        # 此实例原有的 protocol、端口、tls: 1 等配置保留
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

省略 `tls_settings.alpn` 时，回落入站默认使用 `[http/1.1]`。需要 HTTP/2 回落时，显式设置 `[h2, http/1.1]` 并提供能处理 h2c 的对应后端。尽量保留一条不指定 `name`、`alpn`、`path` 的默认规则。

只写端口号时 Xray 会连接 `localhost`，可能解析到 IPv6；后端只监听 IPv4 时，请像示例一样明确填写 `127.0.0.1:端口`。回落目标不能指回代理自身的监听端口，也不要把网站后端开放到公网。

此入口不能与 VLESS 的额外 `decryption` 加密一起使用。不支持的内核、协议、传输、Reality 组合及无效参数会在加载配置时明确报错。`kernel.custom_config` 仍不合并自定义入站；请使用上述专用配置。

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
