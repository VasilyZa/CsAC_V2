# 站点管理与举报

[返回 API 手册](../api.md) · [群管理入口](groups.md#成员操作) · [消息撤回](chat.md#撤回与精华)

`admin.*` 使用 `admin` 访问级别；账号的站点身份读取 `account.self.site_admin` 或 WebSocket `hello.site_admin`，与群角色分开；站点允许配置多个管理员，UID 本身不授予权限

## 举报和故障反馈

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `reports.submit` | `member` | 是 | `{target_kind,target_id,reason,anonymous?}` | `{report_id}` |
| `reports.bug` | `member` | 是 | `{title,description}` | `{report_id}` |
| `admin.reports.list` | `admin` | — | `{state?,before_id?,limit?}` | 直接返回 `Report[]` |
| `admin.reports.review` | `admin` | 是 | `{report_id,state,note?}` | `{report_id,state,note}`，同时通知举报人 |

- 举报目标 `target_kind` 为 `user` 或 `group`，`target_id` 分别用账号 UID 或群的 `conversation_id`；`reason` 去掉两侧空白后至少 10 个 Unicode 码点，最多 12,000 UTF-8 字节；`anonymous` 默认 `false`
- 故障反馈的 `title`、`description` 去掉两侧空白后非空，分别最多 256 和 12,000 UTF-8 字节；管理端显示 `target_kind: "bug"`，描述在 `reason`，没有 `target_id`
- 列表默认 20 条，范围 1–30，按 `report_id` 降序，`before_id` 排除该 ID；`state` 可省略查全部，也可为 `pending`、`processing` 或 `resolved`；没有下一页字段，满页时用最后一项的 `report_id` 翻页，直到少于请求数量
- 审核只可将状态写成 `processing` 或 `resolved`，`note` 默认空字符串，最多 4,000 UTF-8 字节；提交前核对状态，换新的 `key` 再审核是一次新操作，并可再生成一条通知

`Report` 包含 `report_id`、`reporter_id?`、`target_kind`、`target_id?`、`title?`、`reason`、`anonymous`、`state`、`note?`、`created_at`；匿名举报在管理列表中不返回 `reporter_id`，审核结果仍会由服务端发给提交者；普通客户端在[通知](friends-notices.md#通知类型)查看审核结果，不通过管理列表查自己的举报

## 账号与群组处置

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `admin.account.ban` | 是 | `{user_id,reason,ban_days?}` | `{user_id,status:"banned",ban_until}` |
| `admin.account.unban` | 是 | `{user_id}` | `{user_id,status:"active"}` |
| `admin.account.restore` | 是 | `{user_id}` | `{user_id,status:"active"}`，删除后的 14 天冷静期内恢复 |
| `admin.group.ban` | 是 | `{conversation_id,ban_days,reason}` | `{conversation_id,banned:true,ban_until,reason}` |
| `admin.group.unban` | 是 | `{conversation_id}` | `{conversation_id,banned:false,ban_until:null}` |
| `admin.bans.list` | — | `{kind?,before_id?,limit?}` | `{items: Ban[],next_before_id?}` |

`reason` 去掉两侧空白后为 1–500 个 Unicode 码点，不含控制字符；限时封禁用 1–36,500 个 24 小时日；仅账号可以用 `ban_days: 0` 或省略表示无限期，其 `ban_until` 为 `null`

账号封禁撤销目标已有会话；不能用这组命令处置站点管理员；已删除账号需用恢复而非解封；解封与恢复不恢复旧会话，用户重新登录；群封禁暂停普通群聊读写、输入状态与新增成员，群资料仍可展示封禁时间和原因，群主会收到通知；站点管理员保留群历史审核和撤回权限；解封不删除群聊，正常访问按当前成员权限恢复

`admin.bans.list` 的 `kind` 默认 `group`，可改 `account`；仅返回仍生效的封禁，按目标 `id` 降序；分页默认 30、范围 1–100，末页省略 `next_before_id`；`Ban` 有 `{id,title,ban_until,reason,username?,owner_id?,owner_nickname?}`：账号条目的 `id` 是 UID、`title` 是昵称，并带 `username`；群条目的 `id` 是会话 ID、`title` 是群名，并带群主信息

## 注册码

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `admin.registration_code.mint` | — | `{max_uses,expires_at}` 或 `{max_uses,never_expires:true}` | `CodeInfo` 加 `code`，即用于注册的完整注册码 |
| `admin.registration_code.list` | — | `{before_id?,limit?}` | `{codes: CodeInfo[],next_before_id}`，注册码 ID 降序，不含完整码 |
| `admin.registration_code.detail` | — | `{id,before_user_id?,limit?}` | `{registration_code,redemptions,next_before_user_id}`；`registration_code` 包含 `code`，兑换记录按 UID 降序 |
| `admin.registration_code.revoke` | 是 | `{id}` | 不含完整码的 `CodeInfo`，保留首次吊销时间及执行人 |

`max_uses` 为正的 32 位整数；特别区分注册码的时间单位：`expires_at` 请求是未来的 **Unix 秒整数**，不是毫秒，也不是 RFC 3339 字符串；不设过期就传 `never_expires: true`，二者不同时提交

| `CodeInfo` 字段 | 含义 |
| --- | --- |
| `id`、`status` | 整数注册记录 ID；`active`、`exhausted`、`expired` 或 `revoked` |
| `max_uses`、`used_count`、`remaining_uses` | 可兑换次数、已兑换次数、剩余次数 |
| `created_at`、`expires_at`、`revoked_at` | **Unix 秒**；后两者未设置时为 `null` |
| `created_by`、`revoked_by` | 站点管理员 UID；无吊销人时为 `null` |

`redemptions` 每项含 `{user_id,username,nickname,registered_at}`，`registered_at` 也是 Unix 秒；列表及详情都默认 30 项、范围 1–100，末页游标为 `null`；将 `next_before_user_id` 传为 `before_user_id`，而不是 `before_id`

完整 `code` 交给获邀用户，在 `account.signup` 的 `registration_code` 中兑换；吊销只阻止后续兑换，不收回已注册账号的聊天资格；签发不带幂等键，响应丢失时先查询列表核对，不自动重复签发；配置缺少注册码签发密钥时，签发和读取完整码会返回 `registration_code_unavailable`
