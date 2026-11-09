# 服务端自定义内核配置（custom_config）

`kernel.custom_config` 用于加载服务器本地的 JSON 或 YAML 文件，将 DNS、自定义出站、路由等高级配置合并到节点端生成的内核配置中。**机器模式、面板单节点模式和无面板独立运行均支持，不需要修改面板，也不限于裸核运行。**

配置只影响服务端，不会修改客户端订阅。文件内容必须使用节点实际运行内核的原生格式；Sing-box 与 Xray 不做跨内核翻译。

## 配置位置与基本步骤

编辑服务实际加载的主配置文件。安装脚本默认使用 `/etc/xboard-node/config.yml`；若启动参数 `-c` 指向其他路径，应编辑实际文件。

在已有的 `kernel` 中增加 `custom_config`，保留原有内核、目录及其他配置：

```yaml
kernel:
  type: singbox
  config_dir: /etc/xboard-node/instances/example
  log_level: warn
  custom_config: /etc/xboard-node/custom-singbox.json
```

| 字段 | 作用 |
| --- | --- |
| `config_dir` | 内核工作目录，供缓存、证书等运行文件使用；不会自动扫描其中的 JSON 或 YAML 文件 |
| `custom_config` | 要读取的自定义文件路径；一个实例引用一个文件，文件中可以同时配置 DNS、出站和路由 |

先创建自定义文件，再添加引用。文件顶层应为内核配置对象，例如 `{"dns": {...}}`，不是单独的 DNS 服务器数组，也不需要包在 `kernel` 或 `standalone` 中。YAML 使用相同字段层级。推荐填写绝对路径，相对路径按进程工作目录解析，不保证相对于主配置文件。

服务进程需要具有文件读取权限；容器部署时，路径必须位于容器内并正确挂载。修改主配置或自定义文件后，以 root 执行：

```sh
xbctl restart
xbctl log
```

自定义文件在内核生成配置时加载，没有独立的文件热更新监听；修改后请重启，不要依赖面板用户同步触发生效。

## 三种运行模式

以下片段展示配置位置。已有部署只添加 `custom_config`，不要用示例覆盖原来的面板地址、凭据、节点或用户配置。

### 机器模式

```yaml
instances:
  - id: example-machine
    panel:
      url: https://panel.example.com
    machine:
      machine_id: 1
      token_env: EXAMPLE_MACHINE_TOKEN
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/example-machine
      custom_config: /etc/xboard-node/custom-singbox.json
```

该机器实例发现的节点都会继承这一文件引用，节点各自的工作目录仍自动派生为 `config_dir/node-节点ID`。这不是按面板节点 ID 自动选择不同自定义文件的入口。

**机器模式可能按传输要求切换实际内核，例如 Sing-box 机器遇到 XHTTP 节点时切换为 Xray。** 不要给可能混用两种内核的整台机器统一引用 Sing-box DNS 文件；需要按内核拆分机器绑定或使用独立实例，并分别引用对应格式的文件。

### 面板单节点模式

```yaml
instances:
  - id: example-panel-node
    panel:
      url: https://panel.example.com
      node_id: 1
      token_env: EXAMPLE_NODE_TOKEN
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/example-panel-node
      custom_config: /etc/xboard-node/custom-singbox.json
```

只作用于这个节点实例；面板继续管理节点和用户，本地文件补充服务端内核配置。

### 无面板独立运行

在已有独立实例中，与 `standalone` 同级的 `kernel` 下添加文件引用：

```yaml
instances:
  - id: example-standalone
    kernel:
      type: singbox
      config_dir: /etc/xboard-node/instances/example-standalone
      custom_config: /etc/xboard-node/custom-singbox.json
    standalone:
      enabled: true
      node:
        protocol: shadowsocks
        listen_ip: "::"
        server_port: 8388
        cipher: chacha20-ietf-poly1305
      users:
        - id: 1
          uuid: "replace-with-a-strong-password"
```

`custom_config` 不是 `standalone.node` 的子字段。完整节点配置见 [独立运行指南](STANDALONE.md)。

### 多实例共享与覆盖

多个使用相同内核的实例可以引用同一份文件，**不必为每个节点复制一份 DNS 配置**。各实例的 `config_dir` 仍应独立。

也可将 `kernel.custom_config` 写在主配置文件最外层的 `kernel` 中，作为 `instances` 的默认值；实例内未填写时继承，填写其他路径时以该实例为准。外层只提供默认值，不额外启动节点。不使用 `instances` 时，直接在原有顶层 `kernel` 中添加即可。

对于使用 `nodes:` 的多节点配置，共享 `kernel.custom_config`；也可以在 `nodes[].kernel.custom_config` 为指定节点覆盖文件路径。不要把此覆盖语法用于动态机器模式。

共享文件会在各个内核实例中分别加载，不代表所有节点共享同一个 DNS 缓存或内核进程。

## 示例一：Sing-box 主备 DNS

将以下内容保存为 `/etc/xboard-node/custom-singbox.json`，适用于当前嵌入的 Sing-box 1.14.4：

```json
{
  "dns": {
    "servers": [
      {
        "type": "udp",
        "tag": "dns-primary",
        "server": "1.1.1.1"
      },
      {
        "type": "udp",
        "tag": "dns-backup",
        "server": "8.8.8.8"
      }
    ],
    "rules": [
      {
        "action": "evaluate",
        "server": "dns-primary",
        "timeout": "2s"
      },
      {
        "match_response": true,
        "response_rcode": "NOERROR",
        "action": "respond"
      }
    ],
    "final": "dns-backup",
    "timeout": "5s"
  }
}
```

先查询主 DNS `1.1.1.1`，收到 `NOERROR` 时返回该结果；超时、查询失败或返回其他响应码时使用备用 `8.8.8.8`。这里只按响应码判断，`NOERROR` 的空回答也会被接受。仅将两个服务器列入 `servers` 并不会自动实现这种主备切换。

这配置的是服务端解析代理目标域名所用的 DNS，不会修改客户端设备的系统 DNS。已显式指定的 `domain_resolver`、`route.default_domain_resolver`、`resolve.server` 等设置可能直接选择特定 DNS 并绕过上述规则，应结合已有配置检查。

官方字段说明：[DNS](https://sing-box.sagernet.org/configuration/dns/)、[UDP DNS](https://sing-box.sagernet.org/configuration/dns/server/udp/)、[evaluate / respond](https://sing-box.sagernet.org/configuration/dns/rule_action/#evaluate)。

## 示例二：本地高级出站与路由

本地自定义文件使用完整的内核顶层对象；面板「自定义出站」「自定义路由」输入框则分别接收数组，两者不要直接混填。以下示例与 DNS 配置可以合并在同一文件中。

### Sing-box

```json
{
  "outbounds": [
    {
      "type": "socks",
      "tag": "local-socks-singbox",
      "server": "127.0.0.1",
      "server_port": 1234,
      "version": "5",
      "username": "testuser",
      "password": "testpass"
    }
  ],
  "route": {
    "rules": [
      {
        "inbound": ["vless-in"],
        "action": "route",
        "outbound": "local-socks-singbox"
      }
    ]
  }
}
```

示例把 `vless-in` 入站的流量交给本机已有的 SOCKS5 服务。其他协议替换为实际入站标签；它不会创建新的 SOCKS 监听服务。使用时替换认证信息，无认证时删除 `username`、`password`。

### Xray

Xray 实例改为 `type: xray`，并引用单独的 Xray 自定义文件，例如 `/etc/xboard-node/custom-xray.json`：

```json
{
  "outbounds": [
    {
      "tag": "local-socks-xray",
      "protocol": "socks",
      "settings": {
        "address": "127.0.0.1",
        "port": 1234,
        "user": "testuser",
        "pass": "testpass"
      }
    }
  ],
  "routing": {
    "rules": [
      {
        "localPort": "443",
        "outboundTag": "local-socks-xray"
      }
    ]
  }
}
```

`localPort` 匹配服务端实际入站监听端口，不是客户端订阅连接端口，也不是访问的网站端口；根据节点修改。无认证时删除 `user`、`pass`。Xray 使用 `routing`，Sing-box 使用 `route`，不能交换使用；DNS 字段结构同样不同。

## 实际合并规则与边界

**`custom_config` 按下面的字段规则合并，不是将整份原生文件替换为最终配置，也不是所有字段都递归合并。**

| 配置部分 | Sing-box | Xray |
| --- | --- | --- |
| `dns` | 整体替换 | 整体替换 |
| `outbounds` | 追加到已有出站后面 | 追加到已有出站后面 |
| 路由规则 | `route.rules` 插入到已有规则前面 | `routing.rules` 插入到已有规则前面 |
| 路由其他字段 | `route.rule_set` 追加，其余子字段覆盖 | `routing` 中除 `rules` 外的子字段覆盖，例如 `domainStrategy`、`balancers` |
| 其他支持项 | `endpoints` 追加，`experimental` 整体替换 | 除受保护字段外的其他顶层项按键覆盖，例如 `api`、`transport`；仍须符合内核格式 |
| 不能通过此入口覆盖 | `inbounds`、`log`，以及上面未列出的顶层项 | `inbounds`、`log`、`stats`、`policy` |

本地文件路由先于面板规则匹配。过宽的本地规则会提前接管流量；追加出站不会按标签替换已有出站，应使用不与自动生成或面板下发出站重复的标签。整个 DNS 对象被替换时，应在同一对象内保留仍需要的服务器与规则。

自动生成的入站、用户、限速、统计等仍由节点端管理。不能在文件中填写 `inbounds[].streamSettings.sockopt` 来修改 Xray 入站；也不能依靠此入口任意改写监听端口、用户或 TLS。独立模式的 Xray 回落应使用主配置的 `standalone.node.fallbacks`，见 [独立运行指南](STANDALONE.md#xray-fallback-回落)。

本地原生配置最终由实际嵌入的内核解析；面板自定义配置的引用校验不能等同于这份文件的完整校验。配置路径不存在会被正常服务启动流程的布局检查拒绝；文件读取或解析错误还会出现在日志中。修改后检查 `xbctl log`，不能仅凭服务进程仍在运行就认定所有配置已正确应用。

相关入口：[README](../README.md)、[面板自定义出站](../docs-custom-outbounds.md)、[面板自定义路由](../docs-custom-routes.md)。
