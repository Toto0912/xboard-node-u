# 自定义出站（v1.1.3）

在 **xboard-u → 节点管理 → 编辑节点 → 高级设置 → 自定义出站** 填写 JSON 数组，然后保存高级设置及外层节点。这里是面板设置，不是服务器本地 `config.yml`。面板与 xboard-node-u 均须升级到 v1.1.3。

## Sing-box 原生格式

服务端实际使用 Sing-box 时，可以直接使用官方原生结构，不必套 `protocol/settings`：

```json
[
  {
    "tag": "example-socks-singbox",
    "type": "socks",
    "server": "127.0.0.1",
    "version": "5",
    "server_port": 1234,
    "username": "testuser",
    "password": "testpass"
  }
]
```

示例连接本机 `127.0.0.1:1234` 的 SOCKS5 服务，不是创建一个新的 SOCKS 监听服务。`testuser`、`testpass` 为示例账号密码，使用时替换；无认证时删除 `username`、`password`。标签 `example-socks-singbox` 与下方路由引用配套，修改时须保持一致。

原生 `detour`、`tls`、`transport`、拨号参数等字段保留在原位置，不会被折叠进 settings 或静默删除。`detour`、selector/urltest 的 `outbounds` 与 `default` 引用会按实际标签检查；依赖环会拒绝应用。能否使用某种协议或参数，最终取决于节点端所嵌入的 Sing-box 版本，面板不会使内核不支持的参数自动生效。

## Xray 原生格式

服务端实际使用 Xray 时，直接填写原生 outbound 对象组成的数组：

```json
[
  {
    "tag": "example-socks-xray",
    "protocol": "socks",
    "settings": {
      "address": "127.0.0.1",
      "port": 1234,
      "user": "testuser",
      "pass": "testpass"
    }
  }
]
```

示例采用扁平的 address/port/user/pass 原生写法，连接本机 `127.0.0.1:1234` 的 SOCKS5 服务。`testuser`、`testpass` 为示例账号密码，使用时替换；无认证时删除 `user`、`pass`。标签 `example-socks-xray` 与路由引用配套。旧式 servers/users 写法仍兼容。`streamSettings`、`proxySettings`、`mux`、`sendThrough` 等原生顶层字段也会保留。`proxySettings.tag` 与 `streamSettings.sockopt.dialerProxy` 的引用会检查，完整字段由实际 Xray 内核解析。

## 兼容旧面板格式

已有配置可以继续使用，不需要批量改写：

```json
[
  {
    "tag": "example-socks-singbox",
    "protocol": "socks",
    "settings": {
      "server": "127.0.0.1",
      "server_port": 1234,
      "version": "5",
      "username": "testuser",
      "password": "testpass"
    }
  }
]
```

上面是 **Sing-box 的旧面板格式**：节点端把 protocol 转成 type，展开 settings。旧 `proxy_tag` 转成原生 `detour`。Xray 的旧 proxy_tag 转成 `proxySettings.tag`。不要同时填写 proxy_tag 与原生 proxySettings，也不要在一项中混写 Sing-box 的 type 和旧 protocol/settings。

协议内部参数不是通用的：例如 Xray 的 servers/users 与 Sing-box 的 server/server_port 不会自动互译。切换服务端内核时，须换用对应参数。

## 保存与应用规则

- 只填写出站数组 `[]`，不要粘贴整份配置或外层 `{"outbounds": [...]}`。
- tag 必须非空且唯一；不能与服务器本地自定义出站重复。允许显式替换默认的 direct/block 标签，但请确保相关规则仍能使用。
- 标签引用区分大小写；不允许引用自身、未知标签或构成依赖环。节点端会同时考虑本地配置中的附加出站。
- 面板检查数组、对象和混合格式；面板不掌握运行实例的最终内核和本地全部标签，内核格式与完整引用检查在节点端执行。
- Sing-box 的 type 原生出站不能交给 Xray；Xray 专用顶层参数不能通过旧面板包装交给 Sing-box。不会静默转换或忽略错误格式。
- 保存、重开、REST 和 WebSocket 下发保留原生字段及 `{}` 空对象。仅推送到服务端，不进入客户端订阅。
- 先更新节点端，再保存原生配置。旧格式数据保留，不新增数据库迁移；旧节点端不支持新的原生 Sing-box 出站。

本页主要出站示例采用用户提供的原生配置结构，地址、端口、标签及账号密码均为示例值，不是官方完整示例的逐字复制。

参考：[Sing-box SOCKS](https://sing-box.sagernet.org/configuration/outbound/socks/)、[Sing-box 拨号字段](https://sing-box.sagernet.org/configuration/shared/dial/)、[Xray SOCKS](https://xtls.github.io/config/outbounds/socks.html)、[Xray 出站结构](https://xtls.github.io/config/outbound.html)。
