# 来源、内核补丁与许可证

此文件记录仓库中直接整理的主要组件；完整传递依赖以 `go.mod`、`go.sum` 及各依赖源码中的许可证为准。

| 组件 | 来源及固定版本 | 许可证记录 |
| --- | --- | --- |
| Xboard-Node | https://github.com/cedar2025/Xboard-Node ，开发起点 dev 提交 `0a29338` | 原项目 README 声明 MPL-2.0；根目录 `LICENSE` 补充标准文本，保留原代码作者归属 |
| sing-box | https://github.com/SagerNet/sing-box ，v1.14.4，`c005f35300ce76427dc285481ec5695f5b179cd2` | GPLv3-or-later，及上游对名称/关联声明的附加文字；原样保存在 `licenses/sing-box-LICENSE.txt` |
| Xray-core | https://github.com/XTLS/Xray-core ，v26.10.29，`0afec66085a85191e9dd2a4a6646c7c9a94688d1` | MPL-2.0，见 `licenses/Xray-core-MPL-2.0.txt` |

内核定制补丁来自原节点使用的 fork 功能，并针对上述版本迁移。补丁不替代组件原有许可证或作者声明。
GPLv3 标准全文位于 `licenses/GPL-3.0.txt`，来源为 https://www.gnu.org/licenses/gpl-3.0.txt 。
本仓库保留上游来源，不宣称是 Xboard、sing-box 或 Xray 的官方发行。

`scripts/prepare-singbox.go` 和 `scripts/prepare-xray/` 会下载上述公开源码、核验提交并应用 `patches/` 中的补丁。
生成目录 `third_party/` 不纳入 Git；发布工作流另行生成包含两套已打补丁内核源码的归档。

发布二进制时，请一并保留本文件、许可证以及与二进制对应的节点和内核源码。根目录 MPL 文本不表示所有链接组件均采用 MPL。
其他 Go 依赖的版权与许可证应随具体分发方式保留。不要将测试凭据、真实配置或证书私钥作为源码的一部分发布。
