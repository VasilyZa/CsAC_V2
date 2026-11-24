# 事件推送与断线回放

[返回 API 手册](../api.md) · [WebSocket 协议](protocol.md#websocket) · [接入示例](quickstart.md)

## 事件结构

已启用聊天的连接在 `hello` 或 `session.attach` 完成后接收实时事件；持久事件分配全局序号，临时事件没有 `seq`，也不会出现在回放中；请求响应按 `id` 路由，推送按 `type` 和 `event` 路由

```json
{"type":"event","event":"message.created","seq":"181","at":"2026-01-20T08:30:00.020Z","data":{"conversation_id":42,"seq":"19","sender_id":17,"kind":"text","created_at":"2026-01-20T08:30:00Z","text":"你好"}}
```

| 字段 | 说明 |
| --- | --- |
| `type` | 实时事件为 `event`，回放列表内的事件没有此字段 |
| `event` | 事件名，比如 `message.created` |
| `seq?` | 全局持久事件序号，十进制字符串；示例中 `"181"` 用于同步，而 `data.seq: "19"` 是会话内消息序号 |
| `at` | 发出事件的时刻 |
| `data` | 事件类型对应的负载 |

全局序号对单个账号不是连续的；其他账号的事件、已经失去权限的会话以及已撤回消息的原创建事件都可能被过滤；用服务端给出的游标推进，不用 `seq + 1` 检查缺口

## 游标命令

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `events.head` | `member` | — | `{}` | `{head,retained_through}` |
| `events.replay` | `member` | — | `{after?,limit?}` | `{events,next,head,retained_through,has_more,reset}` |
| `events.confirm` | `member`，仅 WebSocket | — | `{through}`，非负十进制字符串 | `{confirmed_through}`，本连接单调增长的确认位置 |

`events.head` 返回全局最新的 `head` 与已经清理至的 `retained_through`；`events.replay` 返回 `after` **之后**当前账号有权读取的持久事件，按 `seq` 升序；`after` 默认 `"0"`，负数不合法；`limit` 省略或为 `0` 时用 100，其他整数取到 1–100；每页还有字节预算，`has_more` 才是翻页依据

```json
{"id":"replay-1","op":"events.replay","data":{"after":"180","limit":100}}
```

```json
{"id":"replay-1","data":{"events":[],"next":"185","head":"185","retained_through":"80","has_more":false,"reset":false}}
```

`has_more: true` 时，继续用该页 `next` 作为 `after`；`false` 时，`next` 为该页扫描的 `head`，即使 `events` 为空也可能前进；若 `after < retained_through` 或 `after > head`，得到 `reset: true`、空事件数组和当前 `head`，根据下节重新建立快照

`events.confirm` 是可选的连接内确认，`through` 不能超过当前全局 `head`；确认不会持久保存、不会删除事件，也不会替代客户端重连时发送的 `after`

## 首次连接和重连

按实例与账号隔离本地游标和缓存，例如使用 `(api_base,user_id)`；只有事件已应用到本地状态后再提交进度，或在同一笔本地事务写入数据和游标

1. **连接并缓冲**：读取 `hello` 或附加会话结果，核对 UID 和 `chat_enabled`；从连接开始缓冲带 `seq` 的实时事件；打开账号资料等查询时也保持读取循环
2. **初次同步**：没有可用缓存时，取该连接的 `head` 作为基准，再从 `account.self`、会话、好友、群组、通知和空间查询初始状态；以该基准调用回放，补齐查询期间的事件；快照和事件可能描述同一对象，用主键合并，必要时根据事件刷新资源
3. **断线重连**：带原凭据建立新连接，从最后持久化的游标调用 `events.replay`，沿 `next` 读到 `has_more: false`；不要先把已保存游标跳到新 `hello.head`
4. **切回实时**：按全局序号合并回放和缓冲事件，去重；回放区间已由当前权限过滤；重叠区间以回放为准，避免把已撤回、已失权的内容写回；完成回放后按顺序应用超过最终 `next` 的缓冲事件，再处理后续实时帧；消息和其他对象仍以各自资源主键更新
5. **处理重置**：用 `reset` 响应里的 `head` 做新基准，重新查询界面列表、当前会话历史和角标，再回放新基准之后的事件；重建完成前继续缓冲实时推送
6. **区分临时状态**：正在输入和在线变更直接用于界面；重连后重设订阅、重取在线快照，不用旧临时事件更新持久游标

事件默认保留约 7 天；同一个账号的权限变化可导致已有连接关闭或后续事件不可见；处理 `1008` 时用 `session.current` 或 Bearer 请求检查会话，再决定重连还是显示登录

## `presence.watch`

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `presence.watch` | `member`，仅 WebSocket | — | `{user_ids}`，最多 200 个正整数 UID | `{users: Presence[]}`，初始状态快照 |

每次调用**替换**该连接的订阅，`user_ids: []` 取消；首次快照及后续变更使用[在线状态](chat.md#表情提及入口与在线状态)中的可见性和 `{user_id,online,last_active}` 结构；快照返回前就可能收到变更；处理请求时保留重叠变更，按到达顺序应用到快照之后；`presence.changed` 是部分更新，不替换订阅的整个列表；好友关系、屏蔽或群成员变化后重查可见 ID，移除已不可见的条目

## 事件目录

| 事件 | `data` | 接收与更新方式 |
| --- | --- | --- |
| `account.created` | `{user_id}` | 账号自己的创建记录 |
| `account.status_changed` | `{user_id,status}` | 自己的账号状态：`active`、`banned`、`deleted`；连接可能在状态帧送达前结束 |
| `account.sessions_changed` | `{user_id}` | 自己的会话变更，检查 `account.session.list` 或登录状态 |
| `account.password_changed` | `{user_id}` | 自己的密码变更，其他会话被撤销 |
| `account.email_verified` | `{user_id}` | 自己的邮箱验证已更新，刷新账号和聊天权限 |
| `profile.updated` | `{user_id}` | 自己的公开资料变更，刷新 `account.self` |
| `friends.changed` | `{user_id}` | 该 UID 的好友或偏好变更，刷新好友、私聊入口和相关资料 |
| `groups.changed` | `{conversation_id}` | 群组详情或成员关系变化，刷新自己有权读取的列表和资料 |
| `groups.members_changed` | `{conversation_id,user_id}` | 一位成员发生变化，刷新成员名单和角色 |
| `groups.join_requests_changed` | `{conversation_id}` | 面向相关群管理成员，刷新加群审批列表 |
| `group.files_changed` | `{}` | 群文件有变化，刷新正在浏览的目录；事件不包含群或文件 ID |
| `conversation.created` | `{conversation_id}` | 新建私聊的两方，刷新会话列表 |
| `conversation.read` | `{conversation_id,seq}` | 本账号的会话已读推进，更新其他设备，`seq` 是消息序号字符串 |
| `conversation.visibility` | `{conversation_id,hidden}` | 本账号的会话显示状态变化 |
| `message.created` | `Message`，见[消息对象](chat.md#消息对象) | 会话可见成员；按会话 ID 和消息序号合并；系统提示也是此事件 |
| `message.recalled` | `{conversation_id,seq,recall_kind,recalled_at}` | 会话可见成员；替换为撤回占位，并清理引用和精华缓存 |
| `message.featured` | `{conversation_id,seq,featured}` | 会话可见成员；更新精华标志，详情或统计可重新查询 |
| `notice.created` | `{kind,message:{conversation_id,seq}}` | 收到提及或回复通知；`kind` 为 `message.mention` 或 `message.reply`，刷新通知与提及入口 |
| `notices.changed` | `{}` | 通知收件箱发生变化，刷新列表或数量 |
| `space.changed` | `{}` | 空间动态、点赞或评论变化，按当前可见性刷新时间线 |
| `report.created` | `{report_id}` | 面向站点管理员，刷新举报列表或通知 |
| `report.reviewed` | `{report_id,state,note}` | 面向举报提交者，刷新通知 |
| `conversation.typing` | `{conversation_id,user_id,active,ttl_seconds}` | **临时事件**，发送给同实例的其他在线会话成员；`active: false` 立即移除，`true` 在 `ttl_seconds` 后过期 |
| `presence.changed` | `{users: Presence[]}` | **临时事件**，发送给订阅变化用户的连接，列表只包含当前可见的变化条目 |

站点管理员以非成员身份查看群时使用查询命令同步：群查看权限不自动加入该群实时收件范围
