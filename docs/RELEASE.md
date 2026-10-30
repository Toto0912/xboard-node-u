# 发布 xboard-node-u

当前发布仓库为 https://github.com/Toto0912/xboard-node-u 。以下说明也适用于 fork；fork 时替换仓库地址与环境变量。

## 首次推送

首次从源码包建立本地仓库时，先在自己的 GitHub 账户下创建空仓库，不要让 GitHub 再生成 README。已配置 origin 的本地项目可直接推送。

```sh
git remote add origin https://github.com/Toto0912/xboard-node-u.git
git push -u origin main
```

若使用不含 .git 的源码压缩包，请先 `git init -b main`、`git add .` 并自行提交。自己的 fork 应使用实际仓库地址，不要使用原作者地址。
本地准备提交只用于整合源码，原项目和内核作者归属记录在 THIRD_PARTY_NOTICES.md，不表示代码全部由提交者原创。

推送 main 后运行测试和 Linux 构建，不自动发布版本。Pull request 同样不推送镜像或创建 Release。

## 创建版本

建议首次使用预发布标签，例如：

```sh
git tag v0.1.0-beta.1
git push origin v0.1.0-beta.1
```

`v*` 标签通过测试和构建后会创建 GitHub Release，包含：

- xboard-node-linux-amd64 / arm64
- xbctl-linux-amd64 / arm64
- install.sh、LICENSE、THIRD_PARTY_NOTICES.md、licenses.tar.gz
- source-with-kernels.tar.gz（节点和两套已打补丁内核源码）
- SHA256SUMS.txt（上述附件的 SHA256）

含连字符的标签标记为预发布，不占用稳定版 latest。仅有预发布时，请给安装器和 xbctl 指定 `--version v0.1.0-beta.1`。
发布后核对附件和 Actions 日志，再用于正式部署。

标签也构建 ghcr.io/toto0912/xboard-node-u:标签 的 amd64/arm64 镜像；仓库名自动转小写。稳定标签额外更新 latest，预发布不更新 latest。
工作流只使用仓库自带 GITHUB_TOKEN，无需在源码写入访问令牌。首次使用需允许 Actions 运行，且仓库/组织允许工作流写入 Release 和 Packages；GHCR 包的可见性需在 GitHub 中确认。

## 安装与更新的仓库选择

安装器支持本地文件，也支持从显式指定的仓库下载：

```sh
sudo env XBOARD_NODE_REPOSITORY=Toto0912/xboard-node-u bash install.sh \
  --mode machine --panel https://panel.example.com \
  --token 'YOUR_MACHINE_TOKEN' --machine-id 1 --kernel xray \
  --version v0.1.0-beta.1
```

GitHub 工作流设置 REPOSITORY 为 github.repository，嵌入 xbctl 二进制。发布后的 `sudo xbctl upgrade --version v0.1.0-beta.1` 因此使用自己的仓库。
可用 `sudo env XBOARD_NODE_REPOSITORY=Toto0912/xboard-node-u xbctl upgrade --version TAG` 覆盖地址。
用安装器升级且需要联网下载时也必须设置该环境变量，不会从配置文件自动读取仓库。

未指定仓库的本地构建需要显式提供该环境变量；不会回落到原作者仓库。原项目模块路径与来源链接保留，不是发布地址。

## 发布前检查

请确认最终源码、完整内核来源和补丁、许可证文件以及对应版本说明一起发布。
运行 `make test` 与 `make build-all`；检查 git status，避免提交真实 config.yml、.env、密钥、凭据、测试账户或服务器配置。
仓库提供的示例使用占位地址和本地 UUID；实际运行时请自行替换。

`install.sh` 的参数说明可以用 `bash install.sh --help` 查看。
