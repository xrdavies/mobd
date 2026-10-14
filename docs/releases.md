# mobd 发布与安装（macOS Apple Silicon）

此仓库独立构建、签名、公证和发布 mobd。当前发布目标是 macOS 13+ / Apple Silicon。
桌面应用在 [mob](https://github.com/xrdavies/mob) 独立发布，Tauri updater 配置也只在
桌面仓库。两端可共享 R2 桶和下载域名；本仓库仅维护 `mobd/`，无需桌面构建产物。

## 1. 准备 Cloudflare R2

1. 创建专用发布桶，例如 `mob-releases`。桶内只存放允许公开下载的文件。
2. 在桶的 **Settings → Custom Domains** 绑定下载域名，例如
   `download.mob.example.com`。所有示例域名都需要替换成自己的实际域名。
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
以下所有值配置在该环境中；不要提交到 Git，也不要粘贴到公开日志。

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
| `DOWNLOAD_BASE_URL` | 公开 HTTPS 下载域名根地址，如 `https://download.mob.example.com`；不能含路径、认证信息或查询参数 |
| `R2_ACCOUNT_ID` | Cloudflare 的 32 位账户 ID |
| `R2_BUCKET` | 发布桶名称，如 `mob-releases` |

在 **mobd 仓库**单独创建 `release` environment 并配置上述 **8 个 Secrets、3 个 Variables**。
可以复用同一 Developer ID 证书、Apple 公证账号和 R2 桶，但 mob 仓库的 environment
配置不会自动继承；若使用组织级 Secrets，须给 mobd 仓库授权。这里不需要 Tauri 密钥。

```bash
openssl base64 -A -in /path/to/DeveloperID.p12 -out /tmp/apple-certificate.txt
security find-identity -v -p codesigning
```

`GITHUB_TOKEN` 自动提供。构建流程创建草稿 Release 的步骤需要 `contents: write`，
R2 发布只需要 `contents: read`。Apple 凭据缺失或签名、公证失败时不会发布未签名替代品。

## 3. 构建和发布

在 mobd 仓库打标签（与桌面端版本独立）：

```bash
git tag v0.4.0
git push origin v0.4.0
```

也可手动运行 **Release mobd macOS arm64** 并填写已存在的稳定标签。
`.github/workflows/release.yml` 将标签解析为确切提交，运行 Go vet/test，以 CGO
构建 arm64 二进制并注入版本；验证版本和架构后，执行 Developer ID 签名、Apple
公证和签名验证，打包 `mobd`、LICENSE、README，并生成 `SHA256SUMS` 和草稿 Release。
Node.js 24 只用于 CI 的分发测试；用户运行二进制不需要 Node.js 或 Go。

在另一台 Apple Silicon Mac 上试装，编辑面向用户的 release notes，然后发布稳定
GitHub Release。**Publish mobd downloads to R2** 随之运行：

1. 下载这个 mobd Release 的二进制归档和 `SHA256SUMS`，验证校验和。
2. 生成版本清单、校验文件及固定下载域名和 Apple Team ID 的安装脚本。
3. 拒绝覆盖内容不同的版本文件或将 stable 指向更低版本。
4. 上传缺少的版本文件，经公开 HTTPS 下载并重新验证。
5. 最后更新 `mobd/install.sh`、`mobd/releases.json` 和 `mobd/latest.json`。

发布只读取和写入 `mobd/`；不会读取或覆盖 `mob/` 的更新清单。同一仓库的 R2 发布
任务串行执行，完全相同的版本可以重试；更高版本发布后不再补发旧版本。版本号应高于
该产品已经公开的最高版本。已公开版本文件和说明变化时应发布新版本。

可手动运行 **Publish mobd downloads to R2** 并输入已发布的 `vX.Y.Z` 重试。
默认 `GITHUB_TOKEN` 发布的 Release 事件不会触发另一工作流，遇到这种情况需要手动
运行 R2 发布流程。普通账号或 GitHub 网页发布 Release 会自动触发。

```text
mobd/
  v0.4.0/
    mobd_0.4.0_darwin_arm64.tar.gz
    SHA256SUMS
    release.json
    install.sh
  install.sh
  latest.json
  releases.json
```

下载域名和以上路径保持不变，官网 `mob-site` 仍读取 `mobd/latest.json` 和
`mobd/releases.json`；桌面端和服务端无需同时发布，也无需相同版本号。

## 4. 安装和升级

以下地址在完成配置并发布首个版本后才可使用：

```sh
curl -fsSL https://download.mob.example.com/mobd/install.sh | sh
```

也可以先查看脚本，或指定版本及安装目录：

```sh
curl -fsSL https://download.mob.example.com/mobd/install.sh -o install.sh
less install.sh
sh install.sh --version 0.4.0 --install-dir "$HOME/.local/bin"
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

升级：`mobd stop` → 重新运行安装脚本 → `mobd start`。
`stop` 会关闭登录启动，原先希望登录启动的用户在升级后使用 `mobd start --login`。
安装器只负责安装/替换；日常运行仍使用 mobd CLI。安装时正在运行的前台进程需要手动停止。
裸 CLI 无法 stapling 公证票据，Gatekeeper 查询 Apple 公证记录时可能需要联网。

Codex/Claude 的安装和登录、辅助功能、屏幕录制、屏幕共享仍是办公室端运行前提。
使用普通登录用户，不安装 root 服务。详细配置见 [mobd 文档](../README.md)。

## 首次发布验收

- [ ] mobd 仓库 `release` 环境已配置 8 个 Secrets 和 3 个 Variables。
- [ ] R2 下载域名公开可访问，CORS 允许官网，动态入口绕过缓存。
- [ ] 签名与公证成功，在另一台 Apple Silicon Mac 上试装。
- [ ] 发布后无 GitHub 登录也能下载安装；旧配置和会话保留。
- [ ] 校验失败、服务运行中、重复发布和升级行为符合预期。
- [ ] 官网展示 mobd 真实版本；`mob/latest.json` 未被修改。
