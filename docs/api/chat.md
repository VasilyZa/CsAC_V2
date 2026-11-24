# 会话和消息

[返回 API 手册](../api.md) · [群组](groups.md) · [媒体上传](media.md) · [事件同步](events.md)

聊天命令使用 `member` 访问级别；会话 ID 用 JSON 整数，消息序号用十进制字符串；以 `(conversation_id, seq)` 为消息主键，同一 `seq` 可以出现在多个会话；不要把[全局事件序号](events.md)当成消息序号

## 会话列表与私聊

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `conversations.list` | — | `{limit?,before_activity?,before_id?,include_hidden?}` | `{conversations: Conversation[],has_more}` |
| `chat.direct.open` | 是 | `{user_id}`，对方 UID | `{conversation_id}`；重复打开同一对好友返回原会话 |
| `conversations.hide` | 是 | `{conversation_id,hidden}` | `{conversation_id,hidden}`；只改变调用者的列表显示，`false` 可恢复 |

`conversations.list` 默认 50，范围 1–100；按 `last_activity` 降序，同时间按 `conversation_id` 降序；翻页时成对传最后一项的 `last_activity → before_activity` 和 `conversation_id → before_id`；`include_hidden` 默认 `false`；失去好友关系或任一方屏蔽后，私聊从列表消失，不能读写；重新具备双方可见的好友关系后可继续访问原私聊

| `Conversation` 字段 | 类型与语义 |
| --- | --- |
| `conversation_id`、`kind` | 整数 ID；`direct` 或 `group` |
| `title`、`avatar_id?` | 显示标题；私聊优先用自己的好友备注，群聊用群名；头像为媒体 UUID，未设置时省略 |
| `peer_id?` | 私聊对方的 UID，群聊省略 |
| `head`、`read_through` | 本会话的最新消息序号、自己的已读位置；均为字符串，可为 `"0"` |
| `unread`、`hidden` | 未读数、不在普通列表显示与否；系统提示不计入未读数 |
| `last_activity` | 列表排序时间 |
| `last_kind?`、`preview?` | 最近一条非 `system` 消息的类型和最多 200 字符的正文预览；无内容时省略 |

被站点封禁的群可能仍在成员的会话列表中，此时普通成员不能读取历史，列表的未读数为 `0`，无正文预览

## 发送消息

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `chat.send` | 是 | `{conversation_id,kind,text?,attachment_id?,duration_ms?,reply_to?,mention_ids?,sticker?,target_id?}` | 消息发送回执，按类型附字段，见下文 |

所有类型都提供 `conversation_id` 和 `kind`，其余字段按下表填写；群成员发送前检查单人禁言与全员禁言；站点管理身份本身不等于入群或发言身份

| `kind` | 字段 | 说明 |
| --- | --- | --- |
| `text` | `text` | 非空白正文，最多 8,000 UTF-8 字节，保留传入文本 |
| `image` | `attachment_id`、`text?` | 已上传、调用者拥有的 `purpose: "message"` 图片，MIME 以 `image/` 开头 |
| `voice` | `attachment_id`、`duration_ms`、`text?` | 同用途的音频，MIME 以 `audio/` 开头；时长为 1–3,600,000 毫秒 |
| `file` | `attachment_id`、`text?` | 已上传、调用者拥有的 `purpose: "message"` 文件 |
| `sticker` | `sticker` | 用 `stickers.list` 的 `code`，最多 40 字节；返回消息的 `text` 为该编号 |
| `nudge` | `target_id` | 拍一拍，目标是会话内除发送者以外的成员；服务端生成提示文本，使用发送者资料的 `pat_action` |

任何正文都受 8,000 字节上限；`duration_ms` 仅用于语音，`attachment_id` 仅用于图片、语音和文件；`system` 是服务器发出的时间线类型，客户端不使用它发消息

- **回复**：额外提供 `reply_to` 字符串，为同会话内尚未撤回、非 `system` 的消息 `seq`，如 `"18"`
- **提及**：额外提供 `mention_ids` 整数数组，最多 100 项，后端排序和去重；把展示用的 `@名字` 放入正文，结构化 UID 放入 `mention_ids`；只有会话内成员可被提及；协议没有 `@全体` 专用值；提及自己不会给自己发通知，回复自己也不发通知
- **Markdown**：文字消息以原文本传输；Markdown、代码块、列表和链接由客户端渲染，提及的通知目标仍由 `mention_ids` 决定；渲染时按普通用户内容处理，禁用原始 HTML 或在展示前清理

```json
{"id":"send-7","op":"chat.send","key":"send-message-784b8b82","data":{"conversation_id":42,"kind":"text","text":"@小林 看看 **接口说明**","mention_ids":[18],"reply_to":"18"}}
```

```json
{"id":"send-7","data":{"conversation_id":42,"seq":"19","sender_id":17,"kind":"text","created_at":"2026-01-20T08:30:00Z","member_level":4,"member_title":"青铜","group_experience":{"total":120,"gained":10}}}
```

通常回执是消息主键、`sender_id`、`kind` 和 `created_at`，不包含完整正文；发送拍一拍时回执另外带生成的 `text` 和 `target_id` 等可用字段；群聊回执会给出发送者 `member_level`、`member_title`，并附 `group_experience: {total,gained}`，其中 `gained` 可为 `0`；本次经验不随消息事件广播；客户端可先用回执确认乐观消息，再用 `message.created` 或 `chat.message.get` 填充详情，避免出现重复气泡

## 历史、详情与已读

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `chat.history` | — | `{conversation_id,before?,after?,limit?}` | `{messages: MessageDetails[],has_more}` |
| `chat.message.get` | — | `{conversation_id,seq}` | 单个 `MessageDetails` |
| `chat.read` | 是 | `{conversation_id,seq}`，`seq` 可为 `"0"` | `{conversation_id,seq}`；只前进不倒退，返回实际位置；大于会话 `head` 返回 `invalid` |
| `chat.feature.list` | — | 与 `chat.history` 相同 | 与历史同形，只包含当前精华消息 |

历史默认 50 项，范围 1–100；`before`、`after` 是互斥的排他游标；无游标或 `before` 模式按消息 `seq` 降序，`after` 模式按升序；两个游标都可省略或为 `"0"`；`"0"` 不加过滤条件；传回该页**最后一条**的 `seq` 继续翻页；只要 `has_more: true` 就还有数据，即使本页条数不足 `limit`

### 消息对象

`Message` 是 `message.created` 的负载，`MessageDetails` 在它之上补充回看字段；查历史返回的发送者名字、头像、群名片等级按查询时资料填充

| 字段 | 类型与语义 |
| --- | --- |
| `conversation_id`、`seq`、`sender_id` | 整数会话 ID、字符串序号、整数发送者 UID |
| `kind`、`created_at` | 消息类型与发送时间 |
| `text?` | 正文、表情编号或生成的拍一拍文案；空时省略 |
| `attachment_id?`、`duration_ms?` | 附件 UUID、语音时长 |
| `target_id?`、`reply_to?`、`mention_ids?` | 拍一拍目标 UID、被回复消息的字符串序号、提及的 UID 数组；没有值时省略 |
| `member_level?`、`member_title?` | 发件人在群内的显示等级和头衔，群聊可见 |
| `system_action?`、`system_subject_id?`、`system_subject_name?` | `system` 行的动作、目标用户 UID 和名称；动作有 `feature_added`、`feature_removed` |
| `sender_name?`、`sender_avatar_id?` | 详情字段；昵称和头像 UUID |
| `reply?` | 详情字段；`{seq,recalled,sender_id?,sender_name?,kind?,preview?}`，预览最多 160 字符；正文不可见时只显示占位，不使用旧缓存的正文 |
| `recalled_at?`、`recall_kind?` | 已撤回时给出时间及原因类别，正文、附件、回复和提及不再返回 |
| `can_recall` | 详情字段；按查询时的权限与时限计算，推送对象不带此字段 |
| `featured?`、`featured_by?`、`featured_by_name?`、`featured_at?` | 精华状态及设置者 UID、名称、时间；非精华时这些字段省略 |
| `read_by_recipient?` | 私聊详情中对方是否已读此消息，群聊省略；用于己方发件的送达界面时先检查 `sender_id` |

`chat.read` 的事件只发给自己的连接，用来同步多个设备的已读位置；对方是否已读通过 `chat.message.get` 或 `chat.history` 的 `read_by_recipient` 刷新，没有单独推送给发送者的回执事件

`nudge` 与 `system` 可渲染为居中的会话提示；精华状态变更时会生成一条 `system` 历史消息，既适合显示提示，也可以用 `system_subject_id` 匹配当前用户以显示「你的消息」

## 撤回与精华

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `chat.recall` | 是 | `{conversation_id,seq}` | `{conversation_id,seq,recall_kind,recalled_at}`，原消息变为撤回占位，并移除精华标记 |
| `chat.feature.set` | 是 | `{conversation_id,seq,featured}` | `{conversation_id,seq,featured}`；`featured: true` 添加，`false` 移除 |
| `chat.feature.stats` | — | `{conversation_id,period?,time_zone?}` | `{period,time_zone,count,counts_by_kind,rank,latest_added_at}` |

- 发送者可以在 **2 分钟**内撤回自己的普通消息和拍一拍；`system` 不能撤回；群主、群管理员和站点管理员在群内管理常规消息时不受 2 分钟限制，**管理代撤**限 `text`、`image`、`voice`、`file`、`sticker`
- 群管理员不能撤群主的消息；群主可以撤其他群成员的常规消息，站点管理员可在群里撤任一发送者的常规消息，包括群主；站点级查看和撤回范围是群聊，不开放他人私聊；服务端以请求执行时的权限和时间为准，过期或角色不符返回 `forbidden`
- `recall_kind` 有 `sender`、`admin`、`owner`，少量历史记录的类别为 `unknown`；收到 `message.recalled` 后就地替换并清理相关预览
- 群主、群管理员和站点管理员可以设置群精华；目标须未撤回且不是 `system`；同一状态重复设置不会重复发提示；真实变更依次产生 `message.featured` 和一条系统型 `message.created`
- `chat.feature.stats` 统计当前仍标记为精华且未撤回的消息，按被标记时间筛选；`period` 默认 `all`，还可选 `today`（`time_zone` 的本地日）、`week`（最近 168 小时）、`month`（最近 720 小时）；`time_zone` 默认 `Asia/Shanghai`，也可用 `UTC` 或实例时区数据库支持的区域名称；`counts_by_kind` 含 `text`、`image`、`voice`、`file`、`sticker`、`nudge` 六个计数；`rank` 最多 10 项，含 `rank`、`sender_id`、`sender_name`、`count`，按数量降序及 UID 升序；`latest_added_at` 为该会话最近一次当前精华设置时间（不受所选周期限制），无则 `null`

## 表情、提及入口与在线状态

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `stickers.list` | — | `{after?}`，字符串 `code` 排他游标 | 直接返回 `Sticker[]`，按编号升序，每页最多 200；满页时用末尾 `code` 请求下一页 |
| `chat.attention` | — | `{limit?}`，默认 50，范围 0–100 | `{unread_mentions,unread_replies,mentions,replies,limit}` |
| `chat.typing` | — | `{conversation_id,active}` | `null`；有效状态向其他在线成员推送 `conversation.typing` |
| `chat.presence` | — | `{user_ids?,conversation_id?}` | `{users: Presence[],online_user_ids}` |

`Sticker` 含 `code`、`label`、`fallback_text` 和可选的 `media_id`、`download_path`；若有下载路径就显示图片，否则用 `fallback_text`；将 `code` 传给 `chat.send` 的 `data.sticker`，不把图片路径作为 `text` 发出

提及入口 `chat.attention` 统计自己当前群会话中超过已读位置的提及和回复，不含自己的消息与已撤回内容；`mentions`、`replies` 各自最多 `limit` 条，按时间、会话 ID、消息序号降序；每项含 `conversation_id`、`seq`、`sender_id`、`kind`、`created_at`、`reply_to?`；同一消息可同时进入两类；`limit: 0` 只取总数；用 `chat.message.get` 加载正文，用 `chat.read` 推进已读位置

输入状态在同实例内按发送者和会话节流，开始输入每秒最多广播一次，有效停止立即推送，提示在 `ttl_seconds: 5` 后自动消失；断线时清除输入提示，不回放旧状态

`Presence` 含 `{user_id,online,last_active}`，`last_active` 是时间戳或 `null`；在线表示当前服务实例看到该用户有启用聊天的 WebSocket 订阅，最后活跃时间独立保存，也可能来自登录或最近一次有效请求；`user_ids` 每次至多 200 项，重复值合并；无 `conversation_id` 且省略列表时查自己，有会话 ID 时仅保留本会话内的所列成员；可见范围为自己、未互相屏蔽的好友或与自己同在活跃群的成员；不可见的账号被省略，不当作离线；持续观察使用 `presence.watch`，见[事件同步](events.md#presencewatch)
