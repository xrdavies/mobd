# 客户端配对与认证

mobd 强制设备认证：每个客户端先用一次性邀请配对，之后用自己的长期凭据访问，没有匿名模式。
所有网络入口共用此协议，部署见[连接配置](connection-setup.md)。

## 生命周期

| 对象 | 有效期 | 失效方式 |
| --- | --- | --- |
| 配对链接 / 二维码 | 10 分钟，单次兑换 | 兑换、过期、`link --close`、mobd 重启或认证重置 |
| 客户端凭据 | 长期有效，不自动过期 | `clients revoke`、`clients delete` 或认证重置 |

邀请过期、关闭邀请、mobd 或客户端重启，均不会使已经保存的客户端凭据失效。
地址可长期稳定，但地址本身不是授权。每台设备分别配对，不共用凭据。
丢失客户端安全存储需要重新配对；恢复旧认证备份可能恢复已经撤销的授权。

## 办公室管理

```bash
mobd link --url https://office.example.com
# 局域网直连：提示 HTTP 风险，仍正常生成二维码和链接
mobd link --url http://192.168.1.20:8080
mobd clients list
mobd connections
mobd clients revoke CLIENT_ID
mobd clients delete CLIENT_ID
mobd link --close
mobd auth reset --yes
```

`link` 在终端输出同一邀请的二维码、完整链接和过期时间。URL 必须为 HTTP 或 HTTPS 根入口，
没有用户名、密码、路径前缀、query 或 fragment。支持局域网 IP、Tailscale IP、主机名和 IPv6。
使用 HTTP 时向 stderr 输出风险提示，stdout 照常输出，不要求额外确认。
HTTP 本身不加密邀请兑换、长期凭据或业务数据；没有 Tailscale/SSH 等加密通道时，
能监听网络流量的人可能取得这些信息。mobd 不通过 IP 范围或域名推断连接是否安全，
不自动探测地址、转换协议或启用 HTTPS。公网部署应使用 HTTPS 入口，本机 mobd 可以继续通过 HTTP 接收代理转发。
最多同时保留 32 个未过期邀请，`link --close` 清除所有未兑换邀请。

这些命令只通过 Unix socket 操作正在运行的 mobd，不离线修改认证文件。
默认地址是 `~/.mobd/run/control.sock`，目录权限 0700，socket 权限 0600。
自定义实例使用 `controlSocket` / `MOBD_CONTROL_SOCKET` / `serve --control-socket PATH`；
管理命令使用同一 `MOBD_CONTROL_SOCKET` 或末尾的 `--socket PATH`。
自定义 socket 的父目录必须属于当前用户且不允许其他用户访问。
`mobd status` 通过服务快照中的 socket 检查后台服务，无需客户端凭据。

`clients list` 返回 JSON：`clients` 包含 `id`、`name`、`createdAt`、可选的
`lastSeenAt` 和 `revokedAt`。`connections` 列出活跃请求和长连接的 `id`、`clientId`、
`method`、`path`、`startedAt`，不包含失败认证。客户端名称仅是标签。
`lastSeenAt` 每个客户端最多每分钟写入一次；长连接不另外发送心跳，已配对不等于在线。

撤销保留记录，删除移除记录；两者均拒绝后续请求并中断该客户端的 SSE 和实时桌面会话。
撤销不回滚已完成的动作、不自动取消 agent 任务，也不删除会话、任务或附件。
重置会生成新的 serverId、撤销所有客户端、关闭连接并清空邀请，保留业务数据。
`--yes` 是显式的重置确认，日常重启不需要重置。

## 客户端协议 v1

链接保留用户指定的 HTTP/HTTPS 协议、主机和端口，只有 fragment 携带邀请：

```text
https://office.example.com/pair#invite=INVITATION&serverId=SERVER_ID&v=1
http://192.168.1.20:8080/pair#invite=INVITATION&serverId=SERVER_ID&v=1
```

二维码编码完整链接。客户端读取 fragment 并展示目标地址，用户确认后向同一 origin
发起兑换请求；不要将 fragment 作为 HTTP 请求路径的一部分发送。
`GET /pair` 仅返回固定提示，不读取或消费邀请，没有脚本、第三方资源或自动兑换，
聊天软件预览不会消耗邀请。

**没有未过期、未使用的邀请时，配对关闭**：`GET /pair` 和 `POST /pair` 均返回 **404**
`{"error":"pairing is off: …","code":"pairing_closed"}`，不读取请求体。邀请被兑换、过期或
`mobd link --close` 后即关闭，下一次 `mobd link` 重新打开。客户端应把 `pairing_closed` 显示为
“邀请已失效，请在办公室重新运行 mobd link”，而不是“mobd 不支持配对”。没有 deep link / universal link 注册；客户端支持粘贴链接，手机端另支持扫码。

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
配对打开时，无效、过期或已经兑换的邀请返回 **401**；无效请求返回 **400**；存储失败返回 **503**，不签发凭据。
并发兑换最多成功一次。成功响应丢失时，重新获取邀请，不能重复领取旧凭据。
客户端应比对返回的 serverId 与链接，保存地址、serverId、clientId，凭据放系统安全存储，
删除邀请 secret。serverId 只是标识，不是服务器身份的密码学证明。

后续**每个**业务请求使用同一独立凭据：

```http
Authorization: Bearer RANDOM_DEVICE_CREDENTIAL
```

覆盖 `/health`、`/healthz`、会话、任务、上传、截图、附件、SSE、`/stream` 和 `/stream/ws` 握手。
不接受 URL query（下述票据除外）、cookie、代理身份头或“请求来自本机”作为替代授权。
401 响应带 `WWW-Authenticate: Bearer realm="mobd"`；认证存储不可用返回 503。
收到 401 后保留办公室列表项并显示“需要重新配对”，停止使用旧凭据重试。
`/health` 的 `features` 包含 `pairing`。协议不包含 refresh token、JWT、账号系统或凭据刷新流程。

### 实时桌面票据

浏览器 / WebView 的 WebSocket 无法设置请求头，因此实时桌面的 WebSocket 回退
`/stream/ws` 额外接受短期票据。原生客户端在握手中发送 `Authorization`，无需票据。

票据随 `POST /stream`（带 `Authorization`）的 **201** 响应返回，字段为 `wsTicket`：

```json
{"session": "SESSION_ID", "answer": "<SDP>", "wsTicket": "RANDOM_TICKET", "width": 1280, "height": 720}
```

随后打开 `ws(s)://HOST/stream/ws?session=SESSION_ID&ticket=RANDOM_TICKET`。票据 30 秒内有效、
只能使用一次、只对 `GET /stream/ws` 生效，其他路由带票据仍返回 401；同时提供
`Authorization` 时以请求头为准。
票据绑定签发它的客户端：撤销、删除或认证重置会作废未使用的票据，并中断已打开的连接。
每个客户端最多同时保留 8 个未使用票据，只在内存中保存哈希，重启后失效。Origin 校验对两种方式相同。

### 客户端要求

- 安全存储凭据；HTTP、上传、图片、SSE、WebSocket 都携带 Authorization。
  WebView 原生 EventSource 无法设置请求头时使用原生网络桥接或 fetch 流；
  不能把长期凭据放进 URL，`/stream/ws` 使用上面的票据。
- 断线和网络切换后使用已保存凭据重连；401 进入重新配对流程，不循环重试。
- 支持用户指定的 HTTP 入口并展示传输风险；HTTPS 连接不跳过证书校验。
  不跨 origin 重定向发送邀请或凭据；地址变化需重新核验入口，不自动把旧凭据发送到新地址。

## 存储

`dataDir/auth.json` 保存稳定 serverId 和客户端记录；随机 256 位 token 只保存 SHA-256
哈希。文件权限 0600，先写临时文件并同步，再原子替换；文件锁阻止多个守护进程同时修改。
未兑换邀请仅存内存且只保存哈希，重启后失效。
认证文件损坏会阻止服务启动，不降级为匿名访问。缺失时生成新身份且没有已授权客户端；
删除或丢失认证文件后旧凭据无法访问。运行期间重置请使用管理命令，不手动覆盖文件。

旧的全局密钥 `sharedSecret` / `MOBD_SHARED_SECRET` 不再支持：非空值会使启动失败并提示移除。
本地 agent/MCP 的 `/internal/tasks/{id}/ask` 单独校验按任务 token，客户端凭据不能代替它。
管理路由只注册在 Unix socket，公开 HTTP 端口不提供客户端管理或邀请创建。

被拒绝的配对请求（配对关闭、邀请无效、请求无效）写入 mobd 日志，含来源地址；经隧道或代理时
附加 `CF-Connecting-IP` / `X-Forwarded-For`，仅用于记录，不参与访问判断。日志每秒最多一行，
其余计数后在下一行汇总，不记录邀请密钥。成功配对记录设备名和 clientId。

仅 HTTP OPTIONS 预检、固定的 `GET /pair` 提示、受邀请保护的 `POST /pair`（无邀请时关闭）和按任务认证的
MCP 请求在业务认证中间件之外。`GET /stream/ws?ticket=` 在认证中间件内兑换票据。
CORS 和 WebSocket Origin 校验仍保留，不能代替认证。
凭据代表对同一办公室的完整业务访问权，不提供不同设备的数据隔离或只读角色。
