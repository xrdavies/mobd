# 客户端配对与认证

mobd 已实现强制设备认证，私有网络、Funnel 和中继共用相同协议，没有匿名模式。
桌面和手机客户端的链接处理、扫码、安全存储及凭据传递将在 mob 仓库后续实现；
当前旧客户端无法直接连接此版本。网络部署见 [remote access](remote-access.md)。

## 生命周期

| 对象 | 有效期 | 失效方式 |
| --- | --- | --- |
| 配对链接 / 二维码 | 10 分钟，单次兑换 | 兑换、过期、`link --close`、mobd 重启或认证重置 |
| 客户端凭据 | 长期有效，不自动过期 | `clients revoke`、`clients delete` 或认证重置 |

邀请过期、关闭邀请、mobd 或客户端重启，均不会使已经保存的客户端凭据失效。
地址可长期稳定，但地址本身不是授权。桌面和手机分别配对，不共用凭据。
丢失客户端安全存储需要重新配对；恢复旧认证备份可能恢复已经撤销的授权。

## 办公室管理

```bash
mobd link --url https://office.example.com
mobd clients list
mobd connections
mobd clients revoke CLIENT_ID
mobd clients delete CLIENT_ID
mobd link --close
mobd auth reset --yes
```

`link` 在终端输出同一邀请的二维码、完整链接和过期时间。URL 必须为 HTTPS 根入口，
没有用户名、密码、路径前缀、query 或 fragment；本机调试和 SSH 转发允许
`http://127.0.0.1:8080`、`http://[::1]:8080` 或 `http://localhost:8080`。
直接 LAN/Tailscale IP 的 HTTP 不作为分享入口，请用 HTTPS（例如 Tailscale Serve）。
最多同时保留 32 个未过期邀请，`link --close` 清除所有未兑换邀请。

这些命令只通过 Unix socket 操作正在运行的 mobd，不离线修改认证文件。
默认地址是 `~/.mobd/run/control.sock`，目录权限 0700，socket 权限 0600。
自定义实例使用 `controlSocket` / `MOBD_CONTROL_SOCKET` / `serve --control-socket PATH`；
管理命令使用同一 `MOBD_CONTROL_SOCKET` 或末尾的 `--socket PATH`。
自定义 socket 的父目录必须属于当前用户且不允许其他用户访问。
`mobd status` 通过服务快照中的 socket 检查后台服务，无需客户端凭据。

`clients list` 返回 JSON：`clients` 包含 `id`、`name`、`createdAt`、可选的
`lastSeenAt` 和 `revokedAt`；`connections` 包含活跃请求的 `id`、`clientId`、
`method`、`path`、`startedAt`，可按 clientId 统计连接数。客户端名称仅是标签。
最后认证活动按每个客户端最多每分钟写入一次，时间精度约一分钟；长连接不会另外发送心跳。
已配对不等于在线；`connections` 仅展示当前请求和长连接，不包含失败认证。

撤销保留记录，删除移除记录；两者均拒绝后续请求并中断该客户端的 SSE/VNC。
撤销不回滚已完成的动作、不自动取消 agent 任务，也不删除会话、任务或附件。
重置会生成新的 serverId、撤销所有客户端、关闭连接并清空邀请，保留业务数据。
`--yes` 是显式的重置确认，日常重启不需要重置。

## 客户端协议 v1

链接使用客户端可达的 HTTPS URL，只有 fragment 携带邀请：

```text
https://office.example.com/pair#invite=INVITATION&serverId=SERVER_ID&v=1
```

二维码编码完整链接。客户端读取 fragment 并展示目标地址，用户确认后向同一 origin
发起兑换请求；不要将 fragment 作为 HTTP 请求路径的一部分发送。
`GET /pair` 仅返回固定提示，不读取或消费邀请，没有脚本、第三方资源或自动兑换。
聊天软件预览不会消耗邀请。此版本没有自动唤起 App 的 deep link / universal link 注册，
后续客户端先支持粘贴完整链接和扫描此二维码。

```http
POST /pair
Content-Type: application/json

{"invite":"INVITATION","name":"My iPhone"}
```

成功返回 **201**，`Cache-Control: no-store`：

```json
{
  "serverId": "SERVER_ID",
  "clientId": "CLIENT_ID",
  "token": "RANDOM_DEVICE_CREDENTIAL"
}
```

名称为去除两端空白后 1–128 字节的非空字符串，不允许控制字符。
无效、过期或已经兑换的邀请返回 **401**；无效请求返回 **400**；存储失败返回 **503**，不签发凭据。
并发兑换最多成功一次。成功响应丢失时，重新获取邀请，不能重复领取旧凭据。
客户端应比对返回的 serverId 与链接，保存地址、serverId、clientId，凭据放系统安全存储，
删除邀请 secret。serverId 只是标识，不是服务器身份的密码学证明。

后续**每个**业务请求使用同一独立凭据：

```http
Authorization: Bearer RANDOM_DEVICE_CREDENTIAL
```

覆盖 `/health`、`/healthz`、会话、任务、上传、截图、附件、SSE 和 `/vnc` 握手。
不接受 `X-Mob-Token`、URL query、cookie、代理身份头或“请求来自本机”作为替代授权。
401 响应带 `WWW-Authenticate: Bearer realm="mobd"`；认证存储不可用返回 503。
收到 401 后保留办公室列表项并显示“需要重新配对”，停止使用旧凭据重试。

协议不增加 refresh token、JWT、账号系统或凭据到期刷新流程。
业务 `/health` 仍使用 API v3，`features` 新增 `pairing`；它必须在配对成功之后调用。

### 后续桌面与手机适配清单

- 添加办公室：粘贴链接；手机另支持扫描二维码；确认目标后 POST 兑换。
- 安全存储凭据；HTTP、上传、图片、SSE、WebSocket 都携带 Authorization。
- WebView 原生 EventSource / WebSocket 无法设置该请求头时，使用原生网络桥接或 fetch 流。
  当前 mobd **未提供连接票据接口**，不能把长期凭据放 URL 来绕过客户端限制。
  如果实际客户端必须使用票据，再共同设计短期、单次、限用途的票据协议。
- 断线和网络切换后使用已保存凭据重连；401 进入重新配对流程，不循环重试。
- 不跳过 TLS 证书校验，不跨 origin 重定向发送邀请或凭据。地址变化需重新核验入口，
  不自动把旧凭据发送到新链接的地址。
- 私有网络 / 自管 Funnel / 中继模式仅负责网络引导，复用此协议。
  我们的中继尚未实现，不能在客户端显示为已可用。

## 存储与迁移

`dataDir/auth.json` 保存稳定 serverId 和客户端记录；随机 256 位 token 只保存 SHA-256
哈希。文件权限 0600，先写临时文件并同步，再原子替换；文件锁阻止多个守护进程同时修改。
未兑换邀请仅存内存且只保存哈希，重启后失效。
认证文件损坏会阻止服务启动，不降级为匿名访问。缺失时生成新身份且没有已授权客户端；
删除或丢失认证文件后旧凭据无法访问。运行期间重置请使用管理命令，不手动覆盖文件。

升级需要先移除非空 `sharedSecret` / `MOBD_SHARED_SECRET`，否则启动时给出迁移错误。
空的旧字段可保留，但不会启用匿名模式。工作目录、runner、会话、任务和附件均不迁移或清空。
本地 agent/MCP `/internal/tasks/{id}/ask` 继续单独校验按任务 token，客户端凭据不能代替它。
管理路由只注册在 Unix socket，公开 HTTP 端口不提供客户端管理或邀请创建。

仅 HTTP OPTIONS 预检、固定的 GET /pair 提示、受邀请保护的 POST /pair 和按任务认证的
MCP 请求在业务认证中间件之外。CORS 和 VNC Origin 校验仍保留，不能代替认证。
凭据代表对同一办公室的完整业务访问权，不提供不同设备的数据隔离或只读角色。

## 验证边界

自动测试覆盖邀请过期、预览不消费邀请、并发单次兑换、持久化失败、重启后的凭据与撤销、
认证状态损坏、旧密钥与匿名访问拒绝、本机/代理头不能绕过、SSE/VNC 撤销断连、
设备之间的撤销隔离、认证重置和本机管理权限。已有任务/MCP 回归测试也保留。
真实桌面和手机扫码、安全存储、WebView 凭据传递、跨网 SSE/VNC 性能仍需客户端适配后联调。
