# HY2 带宽策略与 Masquerade

本功能在 v2.2.0 引入。配合 xboard-u v2.2.0 使用，支持机器模式、面板单节点模式及独立模式；不改变 Hysteria 1。

## 面板模式

在面板的 HY2 配置中填写带宽与 Masquerade。启用 CDN 客户端 / 服务端分离时，在内层服务端配置中填写伪装。面板生成服务端配置时会把客户端上下行交换一次；节点端直接使用收到的 `up_mbps/down_mbps`，不再交换。

新面板明确下发 `hy2_settings.bandwidth_mode: auto`：

- 上下行均为 0 或未填：Sing-box 输出 `ignore_client_bandwidth: true`，Xray 选择 BBR，并通过 HY2 `auto` 握手响应让客户端忽略自己的带宽提示。
- 至少一项正值：允许 Brutal 协商，双方都有限制时取较小值，未填方向不增加服务端限制。客户端未提供带宽时仍可使用 BBR。

Mbps 统一按十进制换算。**升级后 Sing-box 此前未生效的正值带宽会生效；请核对原有节点的带宽配置。**

旧面板 / 原版 Xboard 不发送上述标记时保留原协商默认值，不会因为两个带宽为 0 就自动强制 BBR；正值仍按原通信字段映射。面板侧的方向修正需要同时升级面板。Masquerade 不会写入客户端订阅。

## 无面板独立运行

在实际使用的 `config.yml` 中，直接在对应实例的 `standalone.node` 下添加 `hy2_settings`，无需新建另一个 YAML 文件。`up_mbps/down_mbps` 在这里是**服务端视角**：上传对应客户端下载，下载对应客户端上传。

下面是 Sing-box 实例的相关部分，仍需保留原来的用户、端口及有效 TLS 证书配置：

```yaml
kernel:
  type: singbox
standalone:
  enabled: true
  node:
    protocol: hysteria
    version: 2
    server_port: 8443
    up_mbps: 47       # 服务端上传，即客户端下载
    down_mbps: 23     # 服务端下载，即客户端上传
    hy2_settings:
      bandwidth_mode: auto
      kernel: singbox
      masquerade:
        type: proxy
        url: https://www.example.com
        rewrite_host: true
```

两项带宽都删去或置 0，保留 `bandwidth_mode: auto`，即使用 BBR。删去 `masquerade` 恢复默认 404。旧独立配置不增加 `hy2_settings` 则保留原协商策略。

伪装对象必须使用实际运行内核的字段；内核不匹配、无效类型或字段会被明确拒绝。切换成 Xray 时，同时将两处 `kernel` 改为 `xray`，并使用下面的 Xray 对象。

## 完整类型示例

以下 JSON 都是单个 `masquerade` 对象，三种类型任选其一。面板可以直接粘贴；独立模式可把相同字段写成上面的 YAML 对象。

### Sing-box

```json
{"type":"file","directory":"/var/www/example"}
```

```json
{"type":"proxy","url":"https://www.example.com","rewrite_host":true}
```

```json
{
  "type":"string",
  "status_code":200,
  "headers":{"Content-Type":["text/plain; charset=utf-8"]},
  "content":"Hello, world!"
}
```

### Xray

```json
{"type":"file","dir":"/var/www/example"}
```

```json
{
  "type":"proxy",
  "url":"https://www.example.com",
  "rewriteHost":true,
  "insecure":false,
  "xForwarded":false
}
```

```json
{
  "type":"string",
  "statusCode":200,
  "headers":{"Content-Type":"text/plain; charset=utf-8"},
  "content":"Hello, world!"
}
```

这些示例按官方文档和嵌入内核支持的字段整理；`xForwarded` 是当前 Xray 实现支持的附加选项。Sing-box 响应头可使用字符串或字符串数组，Xray 为字符串；显式状态码支持 200–599，不填使用默认 200。代理地址只支持 HTTP(S)，不能带账号、密码或片段。

目录是节点端本地目录，需要存在且可读；回源 URL 需要节点可访问。不要在没有准备目录时直接套用 file 示例。`insecure: true` 会跳过 Xray 回源证书检查，应按需谨慎配置。

Masquerade 只处理当前 UDP QUIC 端口的未认证 HTTP/3 请求，不新开 TCP 网站端口；HY2 的 TLS 证书仍需配置。混淆开启后，普通 HTTP/3 浏览器不能直接访问该端口。

不增加 QUIC 性能调参字段：窗口、超时、MTU 等保持内核默认。Xray 的带宽策略与 Salamander / Gecko 映射由节点端生成，无需在 `custom_config` 中额外设置入站。

## 参考

- [Hysteria 2 完整服务端配置与带宽协商](https://v2.hysteria.network/docs/advanced/Full-Server-Config/#bandwidth)
- [Sing-box Hysteria2 入站](https://sing-box.sagernet.org/configuration/inbound/hysteria2/)
- [Xray Hysteria 传输](https://xtls.github.io/config/transports/hysteria.html#masqobject)
