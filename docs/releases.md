# mobd 发布与安装（macOS Apple Silicon）

此仓库独立构建、签名、公证和发布 mobd。当前发布目标是 macOS 13+ / Apple Silicon。
桌面应用在 [mob](https://github.com/xrdavies/mob) 独立发布，Tauri updater 配置也只在
桌面仓库。两端可共享 R2 桶和下载域名；本仓库仅维护 `mobd/`，无需桌面构建产物。

公开下载根地址为 **https://mob-releases.0xd.app**。
最新稳定版本以 [latest.json](https://mob-releases.0xd.app/mobd/latest.json) 为准。
普通用户直接看[安装和升级](#4-安装和升级)；以下 GitHub/Cloudflare 配置面向维护者。

## 1. 准备 Cloudflare R2

1. 创建专用发布桶，例如 `mob-releases`。桶内只存放允许公开下载的文件。
2. 在桶的 **Settings → Custom Domains** 绑定下载域名 `mob-releases.0xd.app`，
   确认域名状态为 Active。桶名和账户 ID 使用该桶的实际值，不从域名猜测。
3. 创建限定到该桶的 **Object Read & Write** R2 API 凭据，保存 Access Key ID
   和 Secret Access Key。上传使用 S3 API；最终用户不需要任何令牌。
4. 不使用 `r2.dev` 作为正式下载地址，不在下载域名启用登录验证或浏览器挑战。
5. 官网跨域读取 JSON，需要在桶设置 CORS（把域名替换成正式官网）：

```json
[
  {
    "AllowedOrigins": ["https://mob.example.com"],
    "AllowedMethods": ["GET", "HEAD"],
    "AllowedHeaders": ["*"],
    "ExposeHeaders": ["ETag"],
    "MaxAgeSeconds": 3600
  }
]
```

本地连接真实 R2 调试时可临时加入 `http://localhost:4321`；普通本地预览不需要。
curl 不依赖浏览器 CORS，官网获取发布清单需要它。

### 缓存规则

发布器为版本路径设置 `Cache-Control: public, max-age=31536000, immutable`，
为可变入口设置 `Cache-Control: no-store`。在 Cloudflare Cache Rules 中：

- `/mob/v*/*`、`/mobd/v*/*` 可缓存，尊重对象的缓存头。
- `/mob/latest.json`、`/mobd/latest.json`、两端的 `releases.json` 和
  `/mobd/install.sh` **绕过缓存**。不要用全站长 TTL 覆盖它们。
- 首次验证前不要缓存缺失文件的 404；调整 CORS/缓存规则后清理旧缓存。

官方参考：[R2 公共访问](https://developers.cloudflare.com/r2/buckets/public-buckets/)、
[R2 缓存](https://developers.cloudflare.com/cache/interaction-cloudflare-products/r2/)、
[S3 CLI 上传](https://developers.cloudflare.com/r2/examples/aws/aws-cli/)。
工作流已设置 AWS CLI classic transfer client 及按需 checksum，以兼容 R2。

## 2. GitHub Actions 配置清单

在 **Settings → Environments** 创建 `release` 环境，允许版本标签和可信手动运行。
以下所有值配置在该环境中；Secrets 不要提交到 Git，也不要粘贴到公开日志。

### Secrets

| 名称 | 内容 |
| --- | --- |
| `APPLE_CERTIFICATE` | 包含私钥的 Developer ID Application `.p12` 文件的 Base64 |
| `APPLE_CERTIFICATE_PASSWORD` | `.p12` 导出密码 |
| `APPLE_SIGNING_IDENTITY` | 完整签名身份，例如 `Developer ID Application: Your Name (TEAMID)` |
| `APPLE_ID` | 公证使用的 Apple 账号 |
| `APPLE_PASSWORD` | Apple app-specific password |
| `APPLE_TEAM_ID` | 10 位 Apple Developer Team ID；也会作为公开签名身份写入安装脚本 |
| `R2_ACCESS_KEY_ID` | 限定到发布桶的 R2 Access Key ID |
| `R2_SECRET_ACCESS_KEY` | 对应的 R2 Secret Access Key |

### Variables

| 名称 | 内容 |
| --- | --- |
| `DOWNLOAD_BASE_URL` | `https://mob-releases.0xd.app`；不能附加 `/mobd` 路径、认证信息或查询参数 |
| `R2_ACCOUNT_ID` | Cloudflare 的 32 位账户 ID |
| `R2_BUCKET` | 发布桶名称，如 `mob-releases` |

在 **mobd 仓库**单独创建 `release` environment 并配置上述 **8 个 Secrets、3 个 Variables**。
上表在 **Environment variables** 中添加；Apple 凭据和 R2 访问密钥在 **Environment secrets** 中添加。
可以复用同一 Developer ID 证书、Apple 公证账号和 R2 桶，但 mob 仓库的 environment
配置不会自动继承；若使用组织级 Secrets，须给 mobd 仓库授权。这里不需要 Tauri 密钥。

```bash
openssl base64 -A -in /path/to/DeveloperID.p12 -out /tmp/apple-certificate.txt
security find-identity -v -p codesigning
```

`GITHUB_TOKEN` 自动提供。构建流程创建草稿 Release 的步骤需要 `contents: write`，
R2 发布只需要 `contents: read`。Apple 凭据缺失或签名、公证失败时不会发布未签名替代品。

## 3. 构建和发布

配置好 `release` 环境后，在 **mobd 仓库根目录**运行。需要 Node.js 24+、Git 和
`origin` 的推送权限；签名证书由 CI 读取，本机无需安装，也不需要 GitHub CLI。
先提交并推送要发布的修改，让本地 `main` 与远端一致。版本号与桌面端独立：

```bash
git switch main
git pull --ff-only
# 如果有本地提交，先执行 git push origin main
make release VERSION=v0.0.3
```

`v0.0.3` 是版本号示例；执行时选择高于已有标签的版本。
若只需重试已发布版本的 R2 分发，直接使用下方的 R2 工作流，不运行此命令。
脚本 `.github/release/tag.mjs` 会：

1. 校验 `vMAJOR.MINOR.PATCH` 格式，不接受预发布后缀或带前导零的版本。
2. 要求当前分支为 `main`，工作区和暂存区干净（包括未跟踪文件）。
3. 在线读取 `origin/main` 和版本标签，确认当前提交已经推送、没有落后远端。
4. 拒绝本地或远端已有的标签，以及低于远端已有稳定标签的版本。
5. 在当前提交创建附注标签，只推送该标签，触发 **Release mobd macOS arm64**。

也可以直接运行 `node .github/release/tag.mjs v0.0.3`。脚本不自动提交、合并或推送
分支，也不修改源码中的版本号；二进制版本由 CI 根据标签注入。测试、打包和签名在
Actions 中执行。本地可先运行 `make check`；只测试发布工具使用 `make release-test`。

推送标签成功表示构建请求已提交，**不表示构建或公开发布已经完成**。
到仓库的 Actions 页面查看 **Release mobd macOS arm64**；也可以手动运行它，
填写已存在的稳定标签。
`.github/workflows/release.yml` 将标签解析为确切提交，运行 Go vet/test，以 CGO
构建 arm64 二进制并注入版本；验证版本和架构后，执行 Developer ID 签名、Apple
公证和签名验证，打包 `mobd`、LICENSE、README，并生成 `SHA256SUMS` 和草稿 Release。
Release 上传两个附件：`mobd_<版本>_darwin_arm64.tar.gz` 和 `SHA256SUMS`。
归档保留 `LICENSE` 和 `README.md`；安装脚本校验这三个条目，但只安装 `mobd`。
Node.js 用于发布脚本及分发测试；用户运行二进制不需要 Node.js 或 Go。

在另一台 Apple Silicon Mac 上试装，编辑面向用户的 release notes，然后在 GitHub
的 Releases 页面打开草稿，点击 **Publish release**，保持 **Set as a pre-release**
未勾选。脚本不会自动发布草稿。**Publish mobd downloads to R2** 随之运行：

1. 下载这个 mobd Release 的二进制归档和 `SHA256SUMS`，验证校验和。
2. 生成版本清单、校验文件及固定下载域名和 Apple Team ID 的安装脚本。
3. 拒绝覆盖内容不同的版本文件或将 stable 指向更低版本。
4. 上传缺少的版本文件，经公开 HTTPS 下载并重新验证。
5. 最后更新 `mobd/install.sh`、`mobd/releases.json` 和 `mobd/latest.json`。

发布只读取和写入 `mobd/`；不会读取或覆盖 `mob/` 的更新清单。同一仓库的 R2 发布
任务串行执行，完全相同的版本可以重试；更高版本发布后不再补发旧版本。版本号应高于
该产品已经公开的最高版本。已公开版本文件和说明变化时应发布新版本。

### 手动发布或重试 R2

GitHub Release 发布后，分发不需要重新构建或签名，按以下步骤运行：

1. 打开 [Publish mobd downloads to R2](https://github.com/xrdavies/mobd/actions/workflows/publish-r2.yml)。
2. 点击 **Run workflow**，Branch 选择 `main`，`tag` 填写要分发的已发布版本，如 `v0.0.3`；
   前提是该版本已完成上一节的构建和 GitHub Release 发布。
3. 点击绿色 **Run workflow**，等待 `publish` job 成功。
4. 检查 `https://mob-releases.0xd.app/mobd/latest.json` 的版本和安装脚本是否可下载。

安装了 GitHub CLI 的维护者也可以执行：

```bash
gh workflow run publish-r2.yml --repo xrdavies/mobd --ref main -f tag=v0.0.3
```

仅更新安装脚本时使用下一节的 `installer_only`；完整分发会校验版本目录内
所有文件，模板变化导致已发布文件内容不同时会拒绝覆盖。

### 仅更新公共安装脚本

若二进制和版本清单已经发布，只需更新安装脚本，在上述工作流中勾选
`installer_only`，`tag` 填写 **当前 stable 版本**。对应命令（替换示例标签）：

```bash
gh workflow run publish-r2.yml --repo xrdavies/mobd --ref main \
  -f tag=v0.0.2 -f installer_only=true
```

此模式先核对 R2 当前 stable 清单和归档校验和，再更新并验证 `mobd/install.sh`。
它不修改任何版本目录、二进制、Release 附件或版本清单。安装入口是
`https://mob-releases.0xd.app/mobd/install.sh`；版本目录内的脚本固定不变。
普通新版本发布保持 `installer_only` 未勾选。

默认 `GITHUB_TOKEN` 发布的 Release 事件不会触发另一工作流，遇到这种情况需要手动
运行 R2 发布流程。普通账号或 GitHub 网页发布 Release 会自动触发。

```text
mobd/
  v0.0.2/
    mobd_0.0.2_darwin_arm64.tar.gz
    SHA256SUMS
    release.json
    install.sh
  install.sh
  latest.json
  releases.json
```

官网 `mob-site` 读取 `mobd/latest.json` 和
`mobd/releases.json`；桌面端和服务端无需同时发布，也无需相同版本号。

等 R2 工作流成功后，打开 `https://mob-releases.0xd.app/mobd/latest.json` 确认版本，
并按下一节的安装步骤验证下载和签名。

### 失败与重试

- 本地检查失败：根据提示处理工作区、分支或版本；此时尚未创建标签。
- 标签推送失败：保留本地标签，避免丢失或改变发布提交。先用
  `git ls-remote origin refs/tags/v0.0.3` 查看远端；若没有该标签，修复网络或权限后执行
  `git -c push.followTags=false push origin refs/tags/v0.0.3:refs/tags/v0.0.3`（使用实际待发布标签）。
  若标签已存在，查看 Actions；不要重新创建或强制移动标签。
- 构建、签名或公证失败：修正 GitHub 环境配置后，在 Actions 重跑失败任务，
  或手动运行 **Release mobd macOS arm64**，使用原标签。
- 需要修改代码：提交并推送修改，使用新的更高版本标签。
- R2 发布失败：手动运行 **Publish mobd downloads to R2**，填写已发布的原标签；
  保持产物和发布说明不变。已公开版本的内容变化需要新版本。
  若只有公共安装脚本修复，使用 `installer_only` 更新当前 stable 的入口。
- 工作流代码已更新时，在 `main` 上点击 **Run workflow** 并填写版本标签；
  旧运行的 **Re-run jobs** 使用原提交，不会采用新的工作流代码。

## 4. 安装和升级

在 macOS 13+ 的 Apple Silicon Mac 上，以普通登录用户执行：

```sh
curl -fsSL https://mob-releases.0xd.app/mobd/install.sh | sh
```

也可以先查看脚本，或指定版本及安装目录：

```sh
curl -fsSL https://mob-releases.0xd.app/mobd/install.sh -o install.sh
less install.sh
sh install.sh --version 0.0.2 --install-dir "$HOME/.local/bin"
```

脚本使用 macOS 自带的 curl、plutil、tar、shasum 和 codesign，不需要 jq、Python、
Go、Node、Make。检查 macOS 13+ 和 Apple Silicon（含 Rosetta 终端），下载精确版本，
验证 SHA-256、归档条目、Developer ID / Team ID / 程序标识和二进制版本，然后通过同目录
临时文件替换 `mobd`。不使用 sudo，不去除 quarantine，不自动修改 shell 配置。

运行中的 LaunchAgent 或 `mobd serve` 会导致安装停止并提示；先完成任务并停止服务。
配置、服务快照、会话和附件均不改动。安装后：

```sh
export PATH="$HOME/.local/bin:$PATH"
mobd --version
mobd init       # 仅首次；编辑 ~/.mobd/config.json
mobd start
mobd status
```

把 `export PATH="$HOME/.local/bin:$PATH"` 加入 `~/.zshrc`，使新终端也能找到 mobd。
`mobd init` 默认监听 `0.0.0.0:8080`（所有 IPv4 接口）；按内网/VPN部署设置
agent 可执行路径、工作目录。仅通过本机代理或隧道访问时，将 `listen` 改为 `127.0.0.1:8080`。
客户端填写办公室 Mac 的 mobd 地址，下载域名只分发文件。

升级：`mobd stop` → 重新运行安装脚本 → `mobd start`。
`stop` 会关闭登录启动，原先希望登录启动的用户在升级后使用 `mobd start --login`。
安装器只负责安装/替换；日常运行仍使用 mobd CLI。安装时正在运行的前台进程需要手动停止。
mobd 不使用桌面应用的 Tauri updater；升级通过再次运行安装脚本完成。
裸 CLI 无法 stapling 公证票据，Gatekeeper 查询 Apple 公证记录时可能需要联网。

Codex/Claude 的安装和登录、辅助功能、屏幕录制、屏幕共享仍是办公室端运行前提。
使用普通登录用户，不安装 root 服务。详细配置见 [mobd 文档](../README.md)。

## 发布验收清单

以下为每次发布需要核对的清单，不代表所有手动验收已经执行：

- [ ] mobd 仓库 `release` 环境已配置 8 个 Secrets 和 3 个 Variables。
- [ ] R2 下载域名公开可访问，CORS 允许官网，动态入口绕过缓存。
- [ ] 签名与公证成功，在另一台 Apple Silicon Mac 上试装。
- [ ] 发布后无 GitHub 登录也能下载安装；旧配置和会话保留。
- [ ] 校验失败、服务运行中、重复发布和升级行为符合预期。
- [ ] 官网展示 mobd 真实版本；`mob/latest.json` 未被修改。
