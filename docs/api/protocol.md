# 协议、鉴权与错误

[返回 API 手册](../api.md) · [快速接入](quickstart.md) · [事件同步](events.md)

## 命令封套

`POST /v2/call` 的请求体和 `/v2/socket` 的文本消息都使用下列 JSON 封套；下文的命令参数和成功结果指 `data` 内部，不包含外层 `id`

```json
{"id":"send-1","op":"chat.send","key":"f37c3576-8c7e-42ad-a6e3-325453e82601","data":{"conversation_id":42,"kind":"text","text":"你好"}}
```

| 字段 | 类型 | 规则 |
| --- | --- | --- |
| `id` | 字符串 | 必填，1–96 字节，服务端原样回传；同一客户端的在途请求使用不同 ID |
| `op` | 字符串 | 必填，2–4 段以点分隔的小写命令名，每段以 `a-z` 开头，后接小写字母、数字或下划线，总长不超过 64 字节 |
| `key` | 字符串 | `requires_key: true` 的命令必填，8–96 个 ASCII 字符，允许字母、数字及 `_.:-`；适合用随机 UUID |
| `data` | 对象 | 可省略，等同于 `{}`；服务端按命令校验字段，省略不适用的字段 |

HTTP 命令发送 `Content-Type: application/json`，单条命令上限 **64 KiB**，包含整个封套；一个封套只调用一条命令；单个 WebSocket 按接收顺序处理，客户端仍须用 `id` 对应响应，因为事件可穿插在命令响应之间；普通业务命令的执行时限为 12 秒，成功 `data` 上限为 1 MiB；`/v2/call` 和 `/v2/session` 的 JSON 结果带 `Cache-Control: no-store`

```json
{"id":"clock-1","data":{"server_time":"2026-01-20T08:30:00.000Z"}}
```

```json
{"id":"send-1","error":{"code":"forbidden","message":"Conversation is not writable"}}
```

成功响应包含 `data`，失败响应包含 `error`；解析、鉴权或握手阶段的错误有时带空 `id`，有些 HTTP 路由故障由网关或路由器以非 JSON 正文返回；WebSocket 的推送用 `type: "hello"` 或 `type: "event"`，不是命令响应

### 字段约定

- 表格中字段后的 `?` 表示可省略，`类型 | null` 表示显式的可空值；没有参数的命令用 `{}`；除非另行标明，请求中的数字、布尔值和字符串不互相替代
- 用户、群、会话、通知等 ID 在 JSON 中用整数；`media_id` 和 `session_id` 是 UUID 字符串；后端的整数 ID 使用 64 位存储，客户端需要覆盖 64 位范围时，用无损 JSON 解析和编码，保留原始整数
- **事件** `seq`、`head`、`after`、`next`、`retained_through` 和 `confirmed_through` 是十进制字符串；按任意精度整数比较这些字符串，原样传输；**消息** `seq`、`reply_to`、时间线的 `before` / `after`、会话的 `head` 和 `read_through` 也用十进制字符串，但属于各自会话，不能和事件序号混用
- 时间戳用 RFC 3339 字符串，可以带小数秒或偏移；注册码时间、发布请求和其他例外以各章的具体字段类型为准；时长字段以各章标明的秒、分钟或天为单位
- 列表各自定义游标、排序和末页标记，透传该列表的下一页字段；空列表是 `[]`，可空的单个对象按章节处理
- 将业务 `error.code` 用于分支，将 `error.message` 用于显示或诊断；客户端解析响应时保留对新增字段、事件和枚举值的处理空间

### 访问级别

| `access` | 要求 |
| --- | --- |
| `public` | 可以匿名调用；主动提供的 Bearer 凭据仍会被校验 |
| `signed_in` | 有效会话即可，包括还未完成聊天验证的账号 |
| `member` | 有效会话且已满足当前实例的聊天权限，WebSocket `hello.chat_enabled` 为 `true` |
| `admin` | 有效会话且 `site_admin: true`，与群内角色不同 |

角色满足访问级别之后，对象有各自的可见性和操作权限；站点管理员由实例配置指定，普通账号 UID 不代表站点管理员权限

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `system.time` | `public` | — | `{}` | `{server_time}`，UTC 时间；可结合往返时间估算时钟偏移 |
| `system.operations` | `public` | — | `{}` | `Operation[]`，按 `name` 升序；字段为 `name:string`、`access:string`、`writes:bool`、`requires_key:bool`、`websocket_only?:bool` |
| `commands.status` | `signed_in` | — | `{key}`，在 `data` 内 | `{state:"complete",result}` 或 `{state:"unknown"}`；`result` 为原写入的 `data` |

### 幂等与超时

1. 客户端在首次发送前保存 `op`、`data` 和 `key`，同一次用户操作跨连接和传输重试时沿用它们，可以生成新 `id`
2. 同一用户对相同命令、相同参数使用相同 `key` 时，返回已提交的原结果；更改 JSON 对象的键顺序不影响匹配；保留原来的值、数组顺序以及字段是否省略
3. 同一用户对不同命令或参数复用已占用的 `key` 返回 `conflict`；只有带 `requires_key: true` 的操作有这份写入记录
4. 超时或断线后，查询 `commands.status`；`unknown` 可能是尚未提交、已回滚、未执行或记录已过保留期；只在保留窗口内用原命令重试；窗口外按业务资源重新对账；记录与持久事件使用同一默认保留期 7 天，由 `CSAC_EVENT_RETENTION_DAYS` 配置
5. 对不要求 `key` 的写入按业务结果核对，特别是创建登录会话、发送验证码和签发注册码；`rate_limited` 等待后退避，`retry_later` 和 `timeout` 对幂等写入沿用原键

## 鉴权

原生客户端向 `POST /v2/call`、需要鉴权的媒体请求或 WebSocket 握手提供 `Authorization: Bearer <token>`；`token` 从普通 `account.signin` 或 `account.signup` 返回，由服务端签发，是不透明的会话凭据；保存在客户端私有存储，退出时清除本地值；会话默认自签发起 30 天，实例可调整；注销后重新登录获取新令牌，协议没有令牌刷新命令

浏览器标准 `WebSocket` 构造器接受 URL 和子协议，用 Cookie 握手，或在建立匿名连接后用 `session.attach` 显式绑定已有 Bearer；Cookie 登录走下节；Cookie **不用于** `POST /v2/call`，将浏览器登录后的业务命令发到 WebSocket；握手和媒体请求中，显式的 `Authorization` 优先于 Cookie

## 浏览器会话

`POST /v2/session` 使用命令封套，要求有效 `Origin`；从页面使用 `fetch` 发起 `POST`，`credentials: 'include'`，`Content-Type: application/json`；下面是此路由的完整命令列表，请求参数与[账号章节](accounts.md)相同

| 命令 | 成功 `data` | Cookie 行为 |
| --- | --- | --- |
| `account.signin`、`account.signup` | `{user_id}` | 签发 Cookie，JSON 不返回 Token 或会话到期时间 |
| `account.signin.challenge`、`account.signup.challenge`、`account.restore.challenge` | `{expires_in,resend_after}` | 不变 |
| `account.restore` | `{user_id,status:"active"}` | 不登录，调用 `account.signin` 继续 |
| `session.current` | `{user_id}`，请求 `{}`；失效或不存在返回 `401 unauthenticated` | 不变 |
| `account.logout` | `{revoked}`，请求 `{}`；失效或不存在也可执行，此时 `revoked: 0` | 清除 Cookie，撤销有效的当前会话 |

`__Host-csac_session` 带 `Path=/; Secure; HttpOnly; SameSite=Strict` 和会话过期时间，没有 `Domain`，由签发主机独占；登录、会话恢复、Socket 和媒体选用同一 API 主机，尽量同源；跨源前端与 API 要在同一站点内并满足 Origin 白名单；`csac.chat` 和 `ws.csac.chat` 各自持有属于本主机的 Cookie；切换 API 主机时在新主机完成登录

`/v2/session` 上的邮箱挑战拒绝使用顶层 `error.code: "invalid_email_challenge"` 和 HTTP `401`；通用命令走 `/v2/call` 或 WebSocket 时，同一种拒绝在 `data.error` 内，见[邮件验证码](accounts.md#邮件验证码)

### Origin 与 CORS

- WebSocket 握手允许与 API 同源，或与部署中的 `CSAC_ALLOWED_ORIGINS` 精确匹配的来源；比较 scheme、主机和有效端口；原生 Bearer 客户端可以省略 `Origin`；Cookie 握手和 `/v2/session` 要求有效 `Origin`
- `/v2/call`、`/v2/session`、`/v2/media/`、`/v2/releases/` 和表情静态资源支持预检；分别允许对应的 `POST`、`GET` / `HEAD` / `PUT` 或 `GET` / `HEAD`，请求头允许 `Authorization`、`Content-Type` 和 `Range`；合法预检返回 `204`，外加具体 `Access-Control-Allow-Origin`；`/v2/session` 和媒体返回 `Access-Control-Allow-Credentials: true`
- Cookie 媒体上传携带可信 `Origin`；同源的 `GET` / `HEAD` 可通过浏览器的同源请求上下文读取；跨源嵌入通过 `crossorigin="use-credentials"` 或 `fetch` 的 `credentials: 'include'` 获取；详见[媒体鉴权](media.md#下载与权限)

## WebSocket

路径 `/v2/socket`，子协议 `csac.v2`，`Sec-WebSocket-Version: 13`；发送标准客户端掩码的 UTF-8 文本消息，每条消息一个 JSON 对象；服务端不协商 WebSocket 扩展；分片合计仍受 64 KiB 限制，每条最多 1,024 个分片

| HTTP 版本 | 请求 | 成功状态 |
| --- | --- | --- |
| HTTP/1.1 | `GET /v2/socket`，`Upgrade: websocket`、`Connection: Upgrade` 和 RFC 6455 的 `Sec-WebSocket-Key` | `101 Switching Protocols` |
| HTTP/2 | 通过 TLS 协商 `h2`，对端启用 Extended CONNECT，发 `CONNECT`、`:protocol = websocket`、`:scheme = https`、`:authority` 和 `:path = /v2/socket`，见 [RFC 8441](https://www.rfc-editor.org/rfc/rfc8441.html) | `200 OK` |
| HTTP/3 | QUIC / UDP，入口启用 Extended CONNECT，使用同样的伪首部，见 [RFC 9220](https://www.rfc-editor.org/rfc/rfc9220.html) | `200 OK` |

HTTP/2 和 HTTP/3 客户端确认目标端在 SETTINGS 发布 `SETTINGS_ENABLE_CONNECT_PROTOCOL = 1` 后发起请求；下面展示 Extended CONNECT 的字段，不是发送到流中的文本：

```text
:method: CONNECT
:scheme: https
:authority: ws.csac.chat:8443
:path: /v2/socket
:protocol: websocket
sec-websocket-version: 13
sec-websocket-protocol: csac.v2
```

在 CONNECT 流上交换 WebSocket 帧，不发 `Upgrade`、`Connection` 或 `Sec-WebSocket-Key`，请求流与响应流都保持打开；三种握手都要确认服务端选择了 `Sec-WebSocket-Protocol: csac.v2`

使用 CDN 或反向代理时，以实际入口协商的能力为准；需要强制某版本时，选择支持该版本的原生库和入口；浏览器的 `WebSocket` API 不提供指定 HTTP 版本的参数

### `hello` 与会话绑定

协商成功后，服务端发送：

```json
{"type":"hello","version":2,"max_command_bytes":65536,"user_id":17,"chat_enabled":true,"site_admin":false,"head":"180","retained_through":"80","server_time":"2026-01-20T08:30:00Z"}
```

`version` 是 CsAC 应用协议版本；`head` 和 `retained_through` 是全局持久事件的头与保留边界；匿名时省略 `user_id`，两个权限标志为 `false`；`chat_enabled: false` 时 `head` 和 `retained_through` 均为 `"0"`

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `session.attach` | `public`，仅匿名 WebSocket | — | `{token}` | `{user_id,chat_enabled,site_admin,head,retained_through}` |

匿名连接的绑定窗口约 30 秒；账号登录或注册命令返回凭据后，调用 `session.attach` 才会让已有匿名连接变成已登录连接；也可以携带凭据新建连接；已绑定的连接再次 attach 返回 `conflict`；服务端重新校验当前会话和权限，切换账号使用新连接

### 心跳、速率和关闭

- 服务端约每 25 秒发送 WebSocket Ping；客户端库负责 Pong 和控制帧；浏览器由网络栈处理；应用层时钟探测可用 `system.time`，它不是协议心跳
- 单连接命令使用令牌桶：已绑定突发 120 条、恢复 30 条/秒；匿名突发 12 条、恢复每两秒 1 条；额外的账号、验证及功能速率见对应章节；全局握手槽位耗尽返回 HTTP `503 busy`
- 出站队列满时服务端尝试以 `1013` 关闭；客户端用带抖动的指数退避重连，并用本地游标回放；连接断开时将未完成的请求标记为结果待确认，而非直接判定写入失败

| 关闭码 | 含义和客户端动作 |
| --- | --- |
| `1000` | 正常结束，例如成功注销后关闭 |
| `1001` | 服务端关闭，按需重连并回放 |
| `1002` | 帧协议错误，检查客户端库和握手实现 |
| `1003` | 收到二进制消息，改用文本消息 |
| `1007` | 帧正文或关闭原因不是有效 UTF-8 |
| `1008` | 会话状态变化，检查凭据和 `chat_enabled` 再重连 |
| `1009` | 消息体超限，缩小后再发 |
| `1013` | 出站背压，重连并回放 |
| `1006` | 客户端报告连接中断且未收到关闭帧，按断线流程恢复 |

### 错误码

WebSocket 业务错误用同一 JSON `error`，没有每条命令的 HTTP 状态码；下列 HTTP 状态适用于通过命令路由返回的错误，媒体及握手还有各自的 HTTP 状态

| HTTP | `error.code` | 客户端处理 |
| --- | --- | --- |
| `400` | `invalid` | 校验封套、字段、游标和该操作的取值范围 |
| `400` | `invalid_registration_code` | 提示注册码失效或不可用，改用有效注册码或邮箱注册 |
| `401` | `unauthenticated`、`invalid_credentials` | 检查凭据和当前会话，引导登录 |
| `401` | `invalid_email_challenge` | `/v2/session` 的验证码或恢复令牌无效，提示重新输入或申请 |
| `403` | `forbidden`、`verification_required` | 按操作检查资源权限、群角色或账号验证；WebSocket 控制命令也可能用 `forbidden` 表达尚未验证 |
| `403` | `account_deleted`、`account_banned`、`group_banned` | 根据状态显示账号恢复、封禁提示或群暂停 |
| `404` | `not_found` | 操作名未知，或目标已消失、当前账号不可见 |
| `409` | `conflict` | 刷新目标状态；如重试键冲突，核对原始命令 |
| `413` | `response_limit`、`media_too_large` | 缩小查询页或文件 |
| `415` | `content_type`、`unsupported_media_type` | 检查 JSON 的 Content-Type 或上传类型和实际文件字节 |
| `429` | `rate_limited` | 按该命令的限制等待并退避 |
| `500` | `internal` | 记录请求 `id` 和命令名；写入时用重试键对账 |
| `502` / `503` | `mail_unavailable` | 邮件发送失败或未配置邮件服务 |
| `503` | `busy`、`timeout`、`retry_later`、`registration_code_unavailable` | 短暂退避；有重试键时沿用；注册码服务未配置时联系实例维护者 |
