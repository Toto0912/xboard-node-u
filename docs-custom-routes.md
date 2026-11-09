# 自定义路由（v1.1.3）

本文介绍面板下发的路由。若需由服务器本地管理高级路由，可使用 [`kernel.custom_config`](docs/CUSTOM-CONFIG.md)：Sing-box 填写 `route.rules`，Xray 填写 `routing.rules`，本地文件规则优先于面板规则匹配。

在 **xboard-u → 节点管理 → 编辑节点 → 高级设置 → 自定义路由** 填写当前内核的规则数组，保存高级设置及外层节点。这里是在面板配置，不是在服务器本地 `config.yml` 中填写。

原生路由不会跨内核转换。面板与 xboard-node-u 均升级到 v1.1.3 后，节点端会检查实际内核格式及引用，完整参数由当前实际内核解析。

## Sing-box 原生规则

配合出站文档中的 `example-socks-singbox` 使用：

```json
[
  {
    "inbound": [
      "vless-in"
    ],
    "action": "route",
    "outbound": "example-socks-singbox"
  }
]
```

此规则匹配入站标签 `vless-in`，将从该 VLESS 入站进入的流量送往 `example-socks-singbox`，不限制访问目标端口。`inbound` 填写入站标签，不是监听端口。使用其他协议时，替换为该节点的实际入站标签，例如 Trojan 使用 `trojan-in`，Hysteria / Hysteria2 使用 `hysteria-in`。如需进一步限定目标端口或域名，可在同一对象增加 `port`、`domain_suffix` 等原生条件。

逻辑规则、sniff、resolve、route-options 等使用对应版本 Sing-box 的原生格式；例如下方只拒绝指定域名或端口：

```json
[
  {
    "type": "logical",
    "mode": "or",
    "rules": [
      {"domain_suffix": ["example.com"]},
      {"port": 25}
    ],
    "action": "reject"
  }
]
```

不会把 `action/outbound` 改成 Xray 字段，也不会删掉当前面板不认识的原生字段。涉及 DNS 服务器或 rule_set 的规则还需要在服务端配置相应资源；仅填写引用不会自动创建资源。

## Xray 原生规则

配合出站文档中的 `example-socks-xray` 使用：

```json
[
  {
    "localPort": "443",
    "outboundTag": "example-socks-xray"
  }
]
```

localPort 匹配 **本地入站端口**，不是用户访问的网站端口，也不是 SOCKS 出站端口。节点实际监听其他端口时应同步修改。当前 Xray 原生规则不要求 `type: "field"`；旧配置中的这个兼容字段可保留。

Xray 规则用 outboundTag 引用出站，或用 balancerTag 引用负载均衡器。balancerTag 所指的 balancer 必须已经在服务端完整自定义配置中定义，其完整依赖由 Xray 检查；面板规则数组本身不会创建 balancer。

## 旧规则兼容及优先级

已有原生 `custom_routes` 数组保留，仍按照当前内核解释。节点端已有的统一 `custom_route_rules` 接口也继续兼容；其中 match/action 对象与这里的原生规则是不同接口，不要粘贴进同一个原生数组。

规则执行顺序保持不变：

1. 统一结构的 `custom_route_rules`。
2. 原生 `custom_routes`（按填写顺序）。
3. 内置屏蔽规则。
4. 面板常规路由。

未命中时沿用各内核的默认出站。原生路由中的 catch-all 可能覆盖后面的内置规则，请根据目的安排顺序。

## 校验与升级

- 只填写 rules 数组 `[]`，不是整份 `route` / `routing` 配置。
- Xray 的 outboundTag/localPort 不能交给 Sing-box；Sing-box 的 action/outbound 不能交给 Xray。
- 出站引用区分大小写。节点端检查面板与本地附加出站，未知引用拒绝应用；Sing-box 逻辑规则的内部条件也会检查格式。
- 非路由动作（例如 sniff/reject）不强制附加 outbound，原生字段保留。错误的版本专属参数仍可能被实际内核拒绝，不能把面板“保存成功”等同于服务端“应用成功”。
- 只下发服务端，不进入客户端订阅；已有数据无需迁移。旧配置若一直引用不存在的标签，升级后会明确报错，应先修正标签。

参考：[Sing-box 规则](https://sing-box.sagernet.org/configuration/route/rule/)、[Sing-box 动作](https://sing-box.sagernet.org/configuration/route/rule_action/)、[Xray 路由](https://xtls.github.io/config/routing.html#ruleobject)。
