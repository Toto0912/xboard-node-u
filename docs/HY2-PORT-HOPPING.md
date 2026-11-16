# HY2 端口跳跃与自动 UDP 转发

> v2.3.0 新增功能。配合 xboard-u v2.3.0 或在独立配置中设置可选范围字段后生效；旧版 v2.2.1 不会自动配置范围规则。

## 配合面板使用

在新版 xboard-u 的 HY2 节点中填写：

- 连接端口：`60000-65000`。
- 服务端口：`60000-65000`。
- 端口跳跃间隔：`10` 或 `10-30`；留空默认 30 秒。

连接范围和间隔进入客户端订阅。节点收到整数 `server_port: 60000` 和可选 `hy2_settings.port_range: "60000-65000"`，只创建一个 HY2 UDP 入站，将其余端口重定向到 60000。

机器模式和面板单节点模式均使用同一套处理，Sing-box / Xray 共用规则管理模块。旧版或原版面板没有新字段时，不创建自动规则。

普通直连部署通常两侧使用相同范围。不同范围需要实际存在公网 NAT 映射。手动管理时，服务端口填写一个实际监听端口，公网范围由管理员转发；节点不会接管这些手动规则。

## 无面板独立模式

完整示例见 [独立 HY2 范围配置](../examples/standalone-hy2-port-hopping.yml.example)。

在现有配置的对应节点中配置以下内容；这是原 config.yml 的一部分，不需要另建一个内核配置文件：

~~~yaml
standalone:
  enabled: true
  node:
    protocol: hysteria
    version: 2
    listen_ip: "::"
    server_port: 60000
    server_name: "hy2.example.com"
    hy2_settings:
      bandwidth_mode: auto
      port_range: "60000-65000"
  users:
    - id: 1
      uuid: "00000000-0000-4000-8000-000000000001"
~~~

将示例域名、UUID 和证书路径替换为自己的配置。`kernel.type` 可以使用 `singbox` 或 `xray`。

本地 `server_port` 必须是整数，且等于范围首端口。不要将 `60000-65000` 直接写进节点端的 `server_port`。单端口运行时删除 `port_range`，保留原整数端口。

间隔不写在服务端的这个节点中，而是在客户端配置中设置。Sing-box 客户端示例：

~~~json
{
  "server_ports": ["60000:65000"],
  "hop_interval": "10s",
  "hop_interval_max": "30s"
}
~~~

以上仅为出站中的跳跃字段，需合并到现有 HY2 客户端出站。固定 10 秒时删除 `hop_interval_max`；使用范围时不要同时填写 `server_port`。随机上限需要 Sing-box 1.14+。参考：[Sing-box HY2 出站](https://sing-box.sagernet.org/configuration/outbound/hysteria2/)。

## 系统和权限

- 自动规则仅支持 Linux。
- 优先使用可用 nftables，其次 iptables / ip6tables；安装器提供相关依赖检查。
- 需要 NET_ADMIN 等相应权限。受限 LXC、非特权容器可能无法自动配置，宜由宿主机手动转发。
- `::` 默认按双栈管理；需要主机 IPv6、内核和后端可用。
- 云安全组及其他防火墙放行公网 UDP 范围。
- 重定向只作用于入站 UDP；同号 TCP 服务可以继续使用。
- 同一宿主机上的多个受管节点不能使用相互冲突的 UDP 地址／范围。
- 不需要按范围数量创建监听器或逐端口规则，也不会每次心跳重建规则。

方案参考：[Hysteria 官方端口跳跃](https://v2.hysteria.network/docs/advanced/Port-Hopping/)。Hysteria 官方程序自带的自动范围功能，与本项目嵌入的两种内核不是同一项实现。

## 清理与恢复

正常停止、重启、删除机器节点、改回单端口或卸载时，节点清理自己建立的规则。系统服务还包含退出清理，程序启动会检查异常退出残留。

默认运行状态目录是 `/run/xboard-node/port-hopping`，其中记录所属实例和进程身份。运行状态目录不应在不同网络命名空间的容器之间共享。不要在服务运行时手动删除其状态记录。

可在故障进程已经停止后执行：

~~~bash
sudo /usr/local/bin/xboard-node --cleanup-port-hopping
~~~

该命令只清理已停止进程留下的所属规则，保留仍在运行的其他节点。卸载器另使用严格检查：还有活动所属节点或清理失败时，保留程序，避免丢失恢复工具。

不能承诺进程被强制结束或断电时仍能即时执行清理；服务管理器及下次启动负责补充恢复。重启防火墙、改变规则后，可通过 `xbctl restart` 让节点按当前配置重新建立规则。

自动处理不会清空全机防火墙或 conntrack。规则安装失败会明确报错；不会无提示退化为仅监听首端口。范围／首端口变化可能影响已有连接及旧订阅，不承诺所有配置更新都无感切换。

## 原有功能

HY2 的上下行带宽方向、BBR / Brutal、证书、Masquerade、DNS、自定义路由／出站、用户及流量统计继续沿用现有处理。所有范围端口进入同一个入站，不额外创建节点或重复用户计费。
