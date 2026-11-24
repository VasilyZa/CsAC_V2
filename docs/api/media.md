# 媒体与群文件

[返回 API 手册](../api.md) · [发送消息](chat.md#发送消息) · [空间](space.md) · [上传示例](quickstart.md#上传并发送文件)

媒体命令交换元数据，二进制字节使用 HTTPS；上传分为申请、PUT、业务关联三步；媒体 UUID 与群文件的整数 `id`、动态 `id` 和消息 `seq` 是不同标识

## 申请和上传

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `media.prepare` | `member` | 是 | `{purpose,mime,filename,size,as_file?}` | `{id,upload_path,expires_at}`，此处的 `expires_at` 是上传预约截止时间 |
| `media.info` | `public`，并检查对象可见性 | — | `{id}`，媒体 UUID | `MediaObject`，上传完成且当前可读时返回 |

| `purpose` | 用途 | 关联位置 |
| --- | --- | --- |
| `avatar` | 头像位图 | `account.profile.update.avatar_id` 或群资料的 `avatar_id` |
| `message` | 消息图片、语音或文件 | `chat.send.attachment_id` |
| `space` | 动态和评论图片 | `space.create` 或 `space.reply` 的 `media_ids` |
| `group` | 群文件 | `group.file.add.media_id` |

`filename` 为 UTF-8 文件名，1–255 字节，无首尾空白、路径分隔符、控制字符，不能是 `.` 或 `..`；`size` 为正整数，以字节计，部署默认限制 64 MiB，可通过 `CSAC_MAX_FILE_BYTES` 调整；`as_file` 默认 `false`，只在 `purpose: "message"` 可为 `true`，使图片或音频也按通用附件下载和到期；实例可以对群文件及消息通用附件使用 `CSAC_FILE_ALLOWED_TYPES` 扩展名白名单

`mime` 为媒体类型；头像与空间仅接收位图 `image/jpeg`、`image/png`、`image/gif`、`image/webp`、`image/bmp`、`image/x-icon`、`image/vnd.microsoft.icon`；群文件和消息还接收：

- 通用文件：`application/octet-stream`、`application/pdf`、`application/zip`、`application/x-gzip`、`application/gzip`、`text/plain`
- 音频及容器：`application/ogg`、`audio/ogg`、`audio/mpeg`、`audio/wave`、`audio/wav`、`audio/x-wav`、`audio/mp4`、`audio/webm`、`audio/aiff`、`audio/flac`、`audio/aac`、`audio/amr`、`audio/amr-wb`
- 视频容器：`video/mp4`、`video/webm`，作为文件附件发送；`chat.send` 的图片和语音还分别要求实际声明为 `image/*` 或 `audio/*`

预约有效期约 15 分钟；连接同一实例，将 `upload_path` 解析为其 HTTPS 路径，发送 **`PUT` 原始文件字节**，不要用 multipart、Base64、压缩传输或 `Content-Range`；`Content-Type` 建议设为申请时的 `mime`，若提供则必须与申请一致；服务器校验声明、字节长度和实际内容；使用同一账号的 Bearer 或 Cookie，并满足[浏览器规则](protocol.md#origin-与-cors)

```json
{"id":"prepare-1","op":"media.prepare","key":"upload-file-8c75c3ed","data":{"purpose":"message","mime":"text/plain","filename":"hello.txt","size":6,"as_file":true}}
```

上传成功返回 **`201 Created`**，`Location` 是 `download_path`，正文直接是 `MediaObject`，没有 RPC 封套：

```json
{"id":"4e728c20-9e80-4f6f-9f6c-3d5a601e34d8","purpose":"message","mime":"text/plain","filename":"hello.txt","size":6,"as_file":true,"ready_at":"2026-01-20T08:30:00Z","expires_at":"2026-01-23T08:30:00Z","download_path":"/v2/media/4e728c20-9e80-4f6f-9f6c-3d5a601e34d8"}
```

`MediaObject` 中 `ready_at` 为上传完成时间，`expires_at` 为文件对象到期时间或 `null`；群文件和通用消息附件默认自上传成功保留 3 天，由 `CSAC_FILE_RETENTION_DAYS` 调整；`avatar`、`space` 和非通用的聊天图片或音频不设普通文件到期时间；`id`、`purpose`、`mime`、`filename`、`size`、`as_file` 和 `download_path` 其余字段如示例

### 上传重试与错误

- `media.prepare` 使用原 `key` 重试会返回同一个预约；预约过期后，用新 `key` 重新申请
- PUT 断线后先以拥有者调用 `media.info {id}`；对象已就绪时继续业务关联；仍在预约期但未就绪时，可重传完整文件；对已就绪对象重复 PUT 返回 `409 conflict`
- 期待字节数不符或提供 `Content-Range` 时为 `400 invalid`，超过上限为 `413 media_too_large`，MIME 或编码不合规为 `415 unsupported_media_type`；不存在、已过期或非自己的预约为 `404 not_found`
- `/v2/media/` 的文件错误通常是 `{error:{code,message}}`；外层鉴权失败可能使用带空 `id` 的命令错误；HTTP Range、静态路由和代理错误也可用纯文本，客户端先检查 HTTP 状态与响应类型

## 下载与权限

`GET /v2/media/{id}` 返回原始字节；`HEAD` 读取响应头；可用 `Range: bytes=0-1023` 请求片段，有效范围返回 `206` 和 `Content-Range`，无效范围可返回 `416`；资源通常带 `Cache-Control: private, no-store`；头像、图片和音频在允许内联时使用 `Content-Disposition: inline`，群文件及通用附件用 `attachment`，文件名来自元数据

`media.info`、`GET` 和 `HEAD` 在读取时应用同一套对象可见性；知道 UUID 不代表可读

| 对象 | 谁可以读 |
| --- | --- |
| 自己上传且仍有效的对象 | 对象所有者 |
| 已关联的个人头像或公开群头像 | 浏览者可匿名读取服务端当前公开的头像 |
| 私有群头像 | 有聊天权限的该群成员或站点管理员 |
| 消息附件 | 能查看至少一条仍引用它且未撤回的消息的账号；表情目录中关联的对象可供有聊天权限的账号读取 |
| 群文件 | 当前群成员或站点管理员 |
| 空间图片 | 有聊天权限且当前能查看对应动态或评论的账号 |

读取前检查账号及其会话；浏览器通过同源请求或带凭据的跨源请求下载；Cookie 媒体读取如无 `Origin`，服务端认可同源请求，以及浏览器标为同站点或直接导航的页面下载上下文；需要在跨源页面嵌入图片或音频时设置 `crossorigin="use-credentials"`；XHR / `fetch` 设置 `credentials: 'include'`

`not_found` 也可表示没有访问权、对象失效或关联内容已变更；收到撤回、退群等事件时更新附件可见状态；过期对象显示失效，不从旧缓存重新发布访问链接

## 静态资源

- `GET` 或 `HEAD /v2/assets/default-avatar.svg`：默认头像 SVG，可在 `avatar_id` 缺失时使用
- `GET` 或 `HEAD /v2/media/builtin-stickers/{code}`：内置表情资源，以 `stickers.list` 返回的 `download_path` 为准，已内置的图片可匿名访问
- 上述静态资源有公开缓存和 `ETag`，与用户上传对象分开；命令回传的路径相对于 API 的 HTTPS 主机，不相对于 WebSocket URL 或第三方页面

## 群文件

先用 `purpose: "group"` 上传，再关联到群；下列命令都是 `member`；`file_id` 是群文件记录整数 ID，不是媒体 UUID

| 命令 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- |
| `group.file.list` | — | `{conversation_id,folder_id?,before_id?,limit?}` | `{files: GroupFile[],folders: Folder[],next_before_id?}` |
| `group.file.add` | 是 | `{conversation_id,media_id,folder_id?}`，`media_id` 是自己上传的群文件 UUID | `{id}`，新的群文件记录 ID |
| `group.file.move` | 是 | `{conversation_id,file_id,folder_id?}` | `{id}`，原文件记录 ID |
| `group.file.remove` | 是 | `{conversation_id,file_id}` | `{id}`，从群文件列表移除该关联 |
| `group.file.folder` | 是 | `create`：`{conversation_id,action:"create",name}`；`rename`：`{conversation_id,action:"rename",folder_id,name}`；`delete`：`{conversation_id,action:"delete",folder_id}` | `{id}`，文件夹 ID |

`GroupFile` 字段为 `{id,media_id,folder_id,uploader_id,uploader_name,filename,mime,size,expires_at,created_at,download_path}`；`folder_id` 为 `null` 时文件在根目录，`expires_at` 为空表示无到期时间；`Folder` 为 `{id,name}`，全群文件夹按 `id` 升序返回；文件过期后不在列表中

列表省略 `folder_id` 或传 `null` 表示所有目录，`0` 表示只看根目录，正整数表示指定目录；添加和移动省略 `folder_id` 或传 `0` 表示根目录；文件列表按记录 ID 降序，默认 50 条、范围 1–100，`next_before_id` 缺失代表末页；每群至多 200 个文件夹，名称去掉两侧空白后为 1–50 个 Unicode 码点，不含控制字符；删除文件夹会把其中的文件移至根目录

成员可添加文件，成员或站点管理员可读列表；文件上传者、群主、群管理员或站点管理员可移动或移除相应文件；上传者退出群后仍可用原记录 ID 撤下自己的文件，但不能移动；文件夹操作需要群主、群管理员或站点管理员；收到 `group.files_changed` 时刷新当前列表
