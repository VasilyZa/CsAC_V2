# 好友与通知

[返回 API 手册](../api.md) · [账号查询](accounts.md#个人资料) · [群组](groups.md) · [事件同步](events.md)

## 查找与读取好友

`friends.*` 使用 `member` 访问级别；先用 `account.lookup` 按 UID 或精确用户名查到账号，再用 `friends.state` 判定可展示的操作；好友、屏蔽、申请和通知列表默认 `limit: 50`，范围 1–100，按对应 ID 降序；将响应 `next_before_id` 传回 `before_id`；`null` 为末页，首屏省略 `before_id`

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `friends.list` | — | `{blocked?,before_id?,limit?}` | `{items: Friend[],next_before_id}`，游标为 `user_id`；`blocked: true` 查询自己屏蔽的账号，不是双向屏蔽记录 |
| `friends.state` | — | `{user_id}` | `{user_id,is_friend,remark,blocked_by_me,incoming_request_id,outgoing_request_id,can_request}` |
| `friends.requests` | — | `{direction?,status?,before_id?,limit?}` | `{items: FriendRequest[],next_before_id}`，游标为 `request_id` |
| `friends.common_groups` | — | `{user_id,before_id?,limit?}` | `{items: GroupSummary[],next_before_id}`，游标为 `conversation_id`，见[群组对象](groups.md#群组对象) |

- `Person` 由 `{user_id,username,nickname,avatar_id}` 组成，`avatar_id` 为 UUID 或 `null`；`Friend` 在此基础上增加 `remark` 字符串
- `FriendRequest` 包括 `request_id`、`sender_id`、`recipient_id`、`peer: Person`、`message`、`kind`、`status`、`created_at`、`resolved_at`；`peer` 是相对于调用者的对方，未处理时 `resolved_at` 为 `null`；`kind` 为 `add` 或 `recovery`
- `direction` 默认 `incoming`，也可选 `outgoing`；`status` 默认 `pending`，可选 `accepted`、`rejected`、`cancelled`
- `friends.state` 的两个请求 ID 在没有对应待处理请求时为 `null`；`can_request` 是依当前可见状态计算的按钮提示，提交时仍按最新双方关系、屏蔽和配额检查

## 添加、管理与恢复好友

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `friends.request` | 是 | `{user_id,message?}` | `{request_id,status:"pending",kind:"add"}` |
| `friends.recover` | 是 | `{user_id,message?}` | `{request_id,status:"pending",kind:"recovery"}` |
| `friends.respond` | 是 | `{request_id,accept}`，`accept` 必须显式为布尔值 | `{request_id,status,kind}`，状态为 `accepted` 或 `rejected` |
| `friends.delete` | 是 | `{user_id}` | `{changed,deletion_id?}`，删除成功时有 `deletion_id`；原本已不是好友则 `changed: false` |
| `friends.undo_delete` | 是 | `{user_id,deletion_id}` | `{changed:true}`，立即恢复原好友关系 |
| `friends.block` | 是 | `{user_id}` | `{blocked:true}` |
| `friends.unblock` | 是 | `{user_id}` | `{blocked:false}` |
| `friends.remark` | 是 | `{user_id,remark}` | `{remark}`，空字符串清除自己的备注 |

申请附言 `message` 去掉前后空白，最多 500 个 Unicode 码点；普通申请和恢复申请共用过去 24 小时 30 条配额，同一对账号最多一条待处理申请，同一发送方对同一目标的恢复申请在 24 小时内最多一次；接收者处理自己收到的申请；再次处理已决定的申请使用原 `key` 查询原结果，换新键会收到 `conflict`

屏蔽不删除好友记录，但隐藏双方可见好友及私聊入口，取消两人之间的待处理申请，并限制单聊和邀请；解除屏蔽只修改自己的一侧；备注去掉前后空白，最多 64 个 Unicode 码点，只有自己可见，需要双方保持可见的好友关系

**撤销删除**和**申请找回**是两个不同流程：

1. 删除后，双方各收到 `friend.deleted`；`deletion_id` 是删除者自己的通知 ID；通知在 72 小时内可见
2. 删除者在 72 小时内，且通知的 `can_undo` 为 `true` 时，提交 `friends.undo_delete`，沿用原好友关系的建立时间；关系发生其他变化、已屏蔽或出现待处理申请时，撤销入口可失效
3. 任何一方在仍有合格的近期删除记录时，可以用 `friends.recover` 让对方重新确认；成功批准后，有可信原始时间时保留原始时间，否则以新批准时刻为准；如果近期删除记录不可恢复，用普通添加申请

## 通知收件箱

通知命令使用 `signed_in`，账号不具备聊天权限也可以读自己的系统通知

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `notices.list` | — | `{kind?,unread_only?,before_id?,limit?}` | `{items: Notice[],next_before_id}`，游标为通知 `id` |
| `notices.count` | — | `{}` | `{unread,system_unread,friend_requests_pending,friend_deletions_unread,total_unread}` |
| `notices.read` | 是 | `{notification_id,kind?}` 或 `{all:true,kind?}` | `{updated}`，本次变更为已读的条数 |

`kind` 是最多 64 个 Unicode 码点的精确过滤字段，省略或空字符串表示所有类型；`unread_only` 默认 `false`；`Notice` 含 `{id,kind,data,created_at,read_at,can_undo?}`，`data` 是该通知类型的负载，`read_at: null` 表示未读；`can_undo` 仅在 `friend.deleted` 类型返回；在删除发起者自己的通知、当前可直接撤销时为 `true`

`unread` 是所有可见未读通知行数；`system_unread` 排除 `friend.request` 和 `friend.deleted`，`friend_requests_pending` 是独立统计的待处理来信数，`friend_deletions_unread` 是仍在窗口中的未读删除通知数量；总角标 `total_unread = system_unread + friend_requests_pending + friend_deletions_unread`；读通知不等于批准或拒绝申请；处理申请走对应响应命令，服务端会读掉关联的可操作通知

### 通知类型

以下是当前命令生成的 `Notice.kind`，不是 WebSocket 事件名；导入的历史通知也可有其他类型，按时间和负载渲染通用行

| `kind` | `data` 的主要字段 |
| --- | --- |
| `friend.request` | `{request_id,user_id,kind}`，`user_id` 是申请人，内层 `kind` 为 `add` 或 `recovery` |
| `friend.responded` | `{request_id,user_id,status,kind}`，用户是决策人 |
| `friend.deleted` | `{user_id,deleted_by}`，`user_id` 是对方 |
| `message.mention`、`message.reply` | `{conversation_id,seq}`，`seq` 为消息序号字符串；读取时以当前消息权限为准 |
| `group.invited`、`group.joined` | `{conversation_id,request_id,user_id,title}`，用户是邀请人，`title` 是群名 |
| `group.join_responded`、`group.invitation_responded` | `{conversation_id,request_id,user_id,status}`，用户是决策人 |
| `group.kicked`、`group.disbanded` | `{conversation_id,title}` |
| `group.transfer_requested` | `{conversation_id,transfer_id,user_id,title}`，用户是原群主 |
| `group.transfer_responded` | `{conversation_id,transfer_id,status}` |
| `group.banned`、`group.unbanned` | `{conversation_id,title,banned,ban_until,reason?}` |
| `account.banned`、`account.unbanned` | `{user_id,status,ban_until,reason?}` |
| `report.created` | `{report_id}`，面向站点管理员 |
| `report.reviewed` | `{report_id,state,note}`，面向举报提交者 |

收到 `notices.changed`、`notice.created` 或举报相关事件时，用 `notices.count` 更新角标，通知页打开时再刷新列表；单独收到 `notice.created` 时，事件没有通知行的 `id`，用列表中的 `id` 标记已读
