# 群组

[返回 API 手册](../api.md) · [聊天](chat.md) · [群文件](media.md#群文件) · [站点封禁](administration.md#账号与群组处置)

群通过 `conversation_id` 标识，同一个 ID 用于群资料、群聊历史、成员管理和群文件；除 `groups.manage_list` 使用 `admin` 外，本章命令均使用 `member`；这里的 `member` 是站点聊天资格，不等于已经加入特定群；各命令还检查目标群权限

## 群目录和成员列表

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `groups.discover` | — | `{query?,before_id?,limit?}` | `{items: GroupSummary[],next_before_id}`，仅公开且可发现的群 |
| `groups.manage_list` | — | `{query?,before_id?,limit?}` | 与发现列表同形，站点管理员查看全部未解散的群，包括私有、不可发现和封禁群 |
| `groups.list` | — | `{before_id?,limit?}` | 与发现列表同形，仅自己已加入的群 |
| `groups.owned` | — | `{owner_id?,before_id?,limit?}` | 与发现列表同形；省略 `owner_id` 查自己；查其他人仅返回公开且可发现的群 |
| `groups.profile` | — | `{conversation_id}` | 一个 `GroupProfile`；私有群资料仅成员和站点管理员可见 |
| `groups.members` | — | `{conversation_id,after_user_id?,limit?}` | `{items: GroupMember[],next_after_user_id}`，成员或站点管理员可读 |

目录按 `conversation_id` 降序，使用 `next_before_id → before_id`；成员按 `user_id` 升序，使用 `next_after_user_id → after_user_id`；末页游标是 `null`，`limit` 默认 50，范围 1–100；`query` 是去前后空白的群名子串，不区分大小写，最多 80 个 Unicode 码点；空字符串返回所有范围内的群

### 群组对象

| `GroupSummary` 字段 | 含义 |
| --- | --- |
| `conversation_id`、`title`、`description` | 群 ID、群名、描述 |
| `avatar_id` | 群头像 UUID 或 `null` |
| `owner_id`、`owner_nickname` | 群主 UID、昵称 |
| `visibility`、`join_policy` | 群可见性和入群模式，见下节 |
| `member_count` | 群内成员数 |
| `site_ban_until?`、`site_ban_reason?` | 站点封禁正在生效时返回，含截止时间与原因 |

`GroupProfile` 包含全部 `GroupSummary` 字段，并加入下列字段；带 `?` 的字段根据角色或入群模式省略，这与字段存在但值为 `false` 或 `""` 含义不同

| 字段 | 含义 |
| --- | --- |
| `role` | 自己在该群的 `owner`、`admin`、`member`，非成员为 `null`；站点管理员并不改变这个字段 |
| `can_manage` | 群管理员、群主或站点管理员 |
| `can_administer` | 群主或站点管理员，涉及更高一级的群权限 |
| `pending_request_id` | 自己待审的加入申请 ID，无则 `null` |
| `announcement?`、`allow_invite?`、`global_mute?` | 群公告、普通成员是否可邀请、是否开启全员禁言，向群成员及站点管理员展示 |
| `discoverable?` | 当前可发现设置，管理者可见 |
| `invite_code?`、`code_single_use?` | `code` 模式下有效的加入码和是否一次性，管理者可见 |
| `join_question?` | `approval` 模式下的问题，可为空字符串 |

`GroupMember` 包含 `{user_id,username,nickname,avatar_id,role,title,level,joined_at,muted_until,muted}`；`muted_until` 是时间戳或 `null`；`muted` 表示成员单独禁言是否仍有效，不包含全员禁言；`title` 是群内头衔，`level` 为显示等级 1–100；公开群资料可见不代表非成员能读取成员名单

## 创建和设置

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `groups.create` | 是 | `{title,description?,announcement?,avatar_id?,visibility?,join_policy?,discoverable?,code_single_use?,fixed_code?}` | `{conversation_id}`，创建者自动成为群主 |
| `groups.edit` | 是 | `{conversation_id,title?,description?,announcement?,avatar_id?}`，至少修改一项 | `{conversation_id}`，用 `groups.profile` 读实际新值 |
| `groups.settings` | 是 | `{conversation_id,visibility?,join_policy?,discoverable?,allow_invite?,join_question?,global_mute?,code_single_use?,fixed_code?}`，至少修改一项 | `{conversation_id}` |
| `groups.reset_code` | 是 | `{conversation_id}` | `{conversation_id,invite_code}`，仅群主或站点管理员可重置自动签发的加入码 |

资料文本去掉两侧空白；`title` 必须非空，最多 32 个 Unicode 码点；`description` 最多 2,000，`announcement` 最多 4,000，`join_question` 最多 200；后三项用空字符串清除；群头像使用调用者自己的已上传图片对象，`purpose` 为 `avatar`；编辑的 `avatar_id: ""` 清除群头像，省略或 `null` 保持原头像；个人头像的清除规则不同，见[个人资料](accounts.md#个人资料)

- 新建群默认 `visibility: "private"`、`join_policy: "approval"`、`discoverable: true`、`allow_invite: true`、`global_mute: false`、`code_single_use: true`；要让陌生用户在目录中看到并申请，显式使用公开群；`discoverable` 控制公开目录，已知 ID 的公开群仍可读公开资料
- `visibility` 为 `public` 或 `private`；私有群的非成员须通过有效加入码或群邀请入群；`join_policy` 为 `open`、`code`、`approval`、`invite`
- `open` 允许公开群直接加入；`code` 需要精确匹配的加入码，长度最多 128 字节；`approval` 允许公开群提交审批申请；`invite` 只走邀请
- `code_single_use: true` 的自动码在成功自助加入后轮换；可复用的自动码用 `code_single_use: false`；自定义固定码 `fixed_code` 非空白、最多 128 字节、要求 `code` 模式，设置固定码会切到可重用模式；`fixed_code: ""` 清除固定码，必要时生成自动码
- `groups.reset_code` 不覆盖固定码；改固定码用 `groups.settings`；管理者踢人后自动码会轮换，固定码保留
- `allow_invite` 只限制普通群成员的邀请权限；`global_mute` 只对普通群成员生效，仍独立检查单人禁言；切换为非公开或非审批时，未处理的自助审批申请会取消；关闭普通成员邀请时，也会取消对应的待处理邀请

## 加群、邀请和审批

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `groups.join` | 是 | `{conversation_id,code?,answer?}` | `{conversation_id,status,request_id?}`；`joined` 直接加入或已在群中，`pending` 等待审批并带申请 ID |
| `groups.invite` | 是 | `{conversation_id,user_id}` | `{conversation_id,request_id,status}`；`pending` 待回应，`joined` 已自动加入 |
| `groups.join_requests` | — | `{conversation_id,before_id?,limit?}` | `{items: JoinRequest[],next_before_id}`，群管理者读取待处理申请 |
| `groups.invitations` | — | `{before_id?,limit?}` | 同上，当前账号收到的待处理邀请 |
| `groups.respond_join` | 是 | `{conversation_id,request_id,accept}` | `{conversation_id,request_id,status}`；`accepted` 或 `rejected` |

`JoinRequest` 含 `request_id`、`conversation_id`、`group_title`、`user_id`、`username`、`nickname`、`avatar_id`、`inviter_id`、`kind`、`answer?`、`status`、`created_at`；用户字段描述申请者或被邀请人；自助申请 `kind: "application"`，`inviter_id: null`，邀请 `kind: "invitation"`；两种列表都按 `request_id` 降序，`limit` 默认 50、最多 100，末页 `next_before_id: null`

申请群设了 `join_question` 时提供 `answer`，最多 500 个 Unicode 码点；邀请需有权限，目标是其他可用账号且双方未屏蔽；若目标开启 `allow_auto_join`，邀请直接加入，客户端可立即刷新成员；否则由受邀用户响应；自助申请由群管理者响应；批准前服务端重新校验申请状态、邀请人权限和群状态；已处理的申请换新键重复操作时返回 `conflict`，原键返回已提交的原结果

每群最多 5,000 名成员；同一类待处理加群记录最多 500 条；站点封禁时阻止新的加入、邀请与批准，客户端依据 `group_banned` 展示该状态

## 成员操作

根据 `GroupProfile.role`、`can_manage`、`can_administer` 和目标角色决定显示哪些操作；群管理员可以管理普通成员，群主或站点管理员也可以管理群管理员；群主身份通过转让流程变更，成员管理不直接改写群主角色

| 命令 | `key` | 请求 `data` | 成功 `data` 和权限 |
| --- | --- | --- | --- |
| `groups.set_admin` | 是 | `{conversation_id,user_id,admin}`，显式布尔值 | `{role}`，`admin` 或 `member`；群主或站点管理员操作其他非群主成员 |
| `groups.set_title` | 是 | `{conversation_id,user_id,title?,level?}`，至少一项 | `{title,level}`；空头衔恢复默认，最多 16 个 Unicode 码点；`level` 1–100；群主可改自己的头衔和等级，站点管理员可改群主的头衔和等级 |
| `groups.mute` | 是 | `{conversation_id,user_id,minutes}`，整数 0–43,200 | `{muted_until}`，`0` 解除禁言并返回 `null`；不能禁言自己或群主 |
| `groups.kick` | 是 | `{conversation_id,user_id}` | `{conversation_id}`；不能踢自己或群主；会取消受该成员关系影响的待处理邀请和转让 |
| `groups.leave` | 是 | `{conversation_id}` | `{conversation_id}`；成员退出；群主先转让或解散 |
| `groups.disband` | 是 | `{conversation_id}` | `{conversation_id}`；群主或站点管理员解散，群从群列表退出且不能再读取群聊历史 |

站点管理员可以通过 `groups.manage_list → groups.profile → groups.members` 管理未加入的群，查看历史、处理群成员和管理精华，并按[撤回](chat.md#撤回与精华)规则管理群消息；站点角色不增加普通群成员身份，也不代替群主接受或发起所有权转让

## 经验与等级

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `groups.activity` | — | `{conversation_id}` | `{level,earned_level,experience,manual_level,progress,daily}`，仅查自己的群成员进度 |

- `level` 是展示等级，`earned_level` 是经验计算等级，`experience` 是该群累计经验；手动设置显示等级后 `manual_level: true`，经验照常累积，进度仍以 `earned_level` 为准
- `progress` 为 `{current,required}`，单位是经验值，展示时可计算 `current / required`；达到经验等级 100 返回 `null`；`daily` 为 `{earned,limit,resets_at}`，服务端给出当天的额度和下次重置时间
- 有效群消息在每个 15 秒时间段最多记一次，每次 10 点，单日上限 100 点，按 `Asia/Shanghai` 的日界重置；拍一拍不计经验，撤回已发消息不扣除已获得经验；回执的 `group_experience.gained` 以服务端结果为准
- 经验阈值 `floor(3 × (等级 − 1) × (等级 + 18) / 2)`，取等级范围 1–100；显示进度优先直接用响应，不必重算；默认头衔依等级为 1–10 青铜、11–20 白银、21–40 黄金、41–80 铂金、81–100 王者，成员列表和消息给出最终显示值

## 群主转让

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `groups.transfer` | 是 | `{conversation_id,user_id}`，目标为其他现有成员 | `{conversation_id,transfer_id,status:"pending"}`，当前群主发出 |
| `groups.transfer_requests` | — | `{before_id?,limit?}` | `{items: Transfer[],next_before_id}`，自己发出或收到的待处理转让 |
| `groups.respond_transfer` | 是 | `{conversation_id,transfer_id,accept}`，受让人处理 | `{conversation_id,transfer_id,status}`，状态为 `accepted` 或 `rejected` |
| `groups.cancel_transfer` | 是 | `{conversation_id,transfer_id}`，原群主取消 | `{conversation_id,transfer_id,status:"cancelled"}` |

`Transfer` 包含 `{transfer_id,conversation_id,group_title,from_user_id,to_user_id,status,created_at}`；列表按转让 ID 降序，下一页 `before_id` 取 `next_before_id`，默认 50、最多 100，末页为 `null`；每群同时只有一条待处理转让；受让人接受后晋升群主并解除其个人禁言，原群主变为普通成员；成功转让后，群有 28 天冷却期；冷却期间再发起返回 `conflict`
