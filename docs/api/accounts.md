# 账号与邮箱验证

[返回 API 手册](../api.md) · [协议与浏览器会话](protocol.md#浏览器会话) · [注册码管理](administration.md#注册码)

账号字段使用 JSON 整数 `user_id`；原生客户端通过普通命令取得会话 `token`；浏览器在 `/v2/session` 调用注册或登录，API 主机发出 HttpOnly Cookie；站点启用聊天验证时，未验证账号仍可登录、查看自己的资料和绑定邮箱，社交、聊天、空间等 `member` 命令在验证后开放

## 注册与登录

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `account.signup` | `public` | — | `{username,password,nickname,email?,email_code?,registration_code?}` | `SessionResult`；在 `/v2/session` 改为 `{user_id}` 并签发 Cookie |
| `account.signin` | `public` | — | 三选一：`{username,password}`、`{email,password}`、`{email,email_code}` | 同上 |
| `account.signup.challenge` | `public` | — | `{email}` | `{expires_in,resend_after}` |
| `account.signin.challenge` | `public` | — | `{email}` | `{expires_in,resend_after}` |

注册时用户名去掉前后空白，允许 3–32 个 ASCII 字母、数字或 `_@.-`，查找和唯一性不区分 ASCII 大小写，显示保留原拼写；昵称去掉前后空白后为 1–16 个 Unicode 码点，不含控制字符；系统保留用户名和昵称不能用，例如 `admin`、`system`、`官方`；新密码至少 **8 个 Unicode 码点**，以提交的原始字符串交给服务端，不裁剪、不做客户端预哈希

默认注册时提供 `email` 与 `email_code`，或者 **只提供** `registration_code`；邮箱注册直接验证邮箱；注册码注册授予聊天资格但不会绑定邮箱；实例关闭强制验证时可以不带这两个组选项；用户名或邮箱已被占用返回 `conflict`，注册码问题返回 `invalid_registration_code`

密码登录要么填用户名，要么填邮箱；验证码登录只使用已验证邮箱；先申请该用途的验证码；错误密码或未知账号返回 `invalid_credentials`；密码正确但账号在封禁或删除状态时，返回 `account_banned` 或 `account_deleted`；已过期的限时封禁可重新登录

`SessionResult` 如下，适用于通用 HTTP 和 WebSocket 的注册、登录：

```json
{"user_id":17,"token":"example-session-token-from-server","expires_at":"2026-02-19T08:30:00Z","need_guide":true,"needs_email_verification":false}
```

| 字段 | 说明 |
| --- | --- |
| `user_id` | 当前账号 UID |
| `token` | 服务端生成的会话令牌，按不透明字符串保存 |
| `expires_at` | 会话固定过期时间，默认 30 天；请求不会续期 |
| `need_guide` | 新账号或该账号首次登录返回的引导标记；客户端自行决定引导界面 |
| `needs_email_verification` | 当前实例要求聊天验证，账号还没有验证邮箱或注册码授予的资格 |

使用 Cookie 的应用可调用 `account.self` 查询 `needs_email_verification`；Cookie 登录的 JSON 不投影 `need_guide`；首次签发后切到业务 WebSocket，按 `hello.user_id` 核对身份

客户端收到 `rate_limited` 时降低并发、延后重试；单服务进程每分钟限制同源 IP 登录 30 次、同标识及同账号各 10 次、同源 IP 注册 10 次、同账号修改密码 8 次；密码计算并发额度满也会返回该错误

## 邮件验证码

填写规范邮箱地址，不附显示名，最多 255 UTF-8 字节；服务端去掉两侧空白并转为小写；各用途对应不同挑战，不能把注册验证码用于登录或绑定

| 用途 | 申请命令 | 提交命令与字段 | 有效期 |
| --- | --- | --- | --- |
| 注册 | `account.signup.challenge` | `account.signup` 的 `email_code` | 600 秒 |
| 登录 | `account.signin.challenge` | `account.signin` 的 `email_code` | 600 秒 |
| 绑定 | `account.email.challenge` | `account.email.bind` 的 `email_code` | 600 秒 |
| 恢复账号 | `account.restore.challenge` | `account.restore` 的 `restore_token` | 3,600 秒 |

前三类为 6 位数字字符串，恢复令牌为邮件中提供的字符串；申请响应的 `expires_in` 和 `resend_after` 均以秒计，`resend_after` 为 `60`；每个用途对同一邮箱及来源 IP 各有独立的 60 秒间隔和每小时 5 次额度；验证码单次使用，最多 5 次错误猜测；之后重新申请；新申请使提交以最新挑战为准；资格不符合的邮箱也可能返回同样的申请回执，不以回执判断邮箱是否已注册或邮件是否已送达

提交错误或过期挑战有两种响应格式：

- 普通 RPC 返回成功封套 `{"id":"verify-1","data":{"verified":false,"error":"invalid_email_challenge"}}`，HTTP 为 `200`；注册、邮箱登录、绑定、恢复都要先识别这个 `data.error`，再读取成功字段
- `/v2/session` 将注册、验证码登录、恢复的同一拒绝投影为 `{"id":"verify-1","error":{"code":"invalid_email_challenge","message":"Invalid or expired email challenge"}}`，HTTP 为 `401`

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `account.email.challenge` | `signed_in` | — | `{email}` | `{expires_in,resend_after}` |
| `account.email.bind` | `signed_in` | — | `{email,email_code}` | `{email_verified:true}`；已绑定验证邮箱返回 `conflict` |

绑定挑战与当前账号关联；绑定成功后重新查询 `account.self`；已有 WebSocket 若因权限变化关闭，则带同一凭据重新连接，读取新的 `hello.chat_enabled`

## 个人资料

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `account.self` | `signed_in` | — | `{}` | `Profile` 加上 `email`、`email_verified`、`registration_grant`、`site_admin`、`needs_email_verification` |
| `account.lookup` | `member` | — | `{user_id}` 或 `{username}`，二者选一 | `Profile`；按精确用户名查找，不是模糊搜索 |
| `account.profile.update` | `signed_in` | 是 | `{nickname?,bio?,pat_action?,allow_auto_join?,avatar_id?}`，至少一项 | 更新后的 `Profile` |

| `Profile` 字段 | 类型与语义 |
| --- | --- |
| `user_id`、`username` | 整数 UID、用户名 |
| `nickname` | 显示昵称，规则与注册相同 |
| `avatar_id` | `UUID` 或 `null`，默认头像路径见[媒体](media.md#静态资源) |
| `bio` | 个性签名，最多 500 个 Unicode 码点，支持换行和制表符；空字符串清空 |
| `pat_action` | 拍一拍的文案片段，最多 16 个 Unicode 码点；发空字符串恢复 `拍了拍` |
| `allow_auto_join` | 布尔值，控制收到群邀请时是否直接加入 |
| `created_at` | 账号创建时刻 |

`avatar_id` 省略时保持原值，传 `null` 清除，传 UUID 时对象必须已上传、由当前用户拥有，且用途为 `avatar`；`account.self.email` 未绑定时为 `null`；`registration_grant` 表示有注册码授予的资格，`site_admin` 表示站点管理员

## 会话、改密和退出

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `account.session.list` | `signed_in` | — | `{}` | `{sessions}`，近 100 个未过期会话，最近签发的在前 |
| `account.session.revoke` | `signed_in` | — | `{session_id}` 或 `{all:true}` | `{revoked}`，撤销数量；`all` 含当前设备 |
| `account.password.change` | `signed_in` | — | `{old_password,new_password}` | `{updated:true}`，保留当前会话，撤销其他会话和已有的账号绑定挑战 |
| `account.logout` | `signed_in` | — | `{}` | `{revoked}`，撤销当前会话；WebSocket 成功响应后关闭 |

`Session` 包含 `{session_id,current,created_at,expires_at}`，其中 `session_id` 为 UUID，会话列表不返回 Token；Cookie 应用走 `/v2/session` 注销，确保响应清除主机 Cookie；改密采用注册的新密码规则，验证旧密码无需做长度调整或裁剪

## 删除和恢复

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `account.delete` | `signed_in` | — | `{}` | `{user_id,status:"deleted",cooling_period_days:14,restorable_until}` |
| `account.restore.challenge` | `public` | — | `{email}`，删除前验证过的邮箱 | `{expires_in,resend_after}` |
| `account.restore` | `public` | — | `{email,restore_token}` | `{user_id,status:"active"}`，再次登录取得新会话 |

删除立即撤销所有会话；站点管理员须先由实例撤销站点角色才能删除自己的账号；14 天冷静期以 `restorable_until` 为准，期内可用验证过的邮箱或请站点管理执行[恢复](administration.md#账号与群组处置)；冷静期结束后服务端清理账号内容，客户端按新的读取结果更新缓存
