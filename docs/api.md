# CsAC v2 API 开发者手册

这份手册面向 CsAC v2 第三方客户端开发者，覆盖账号、好友、群组、聊天、通知、空间、媒体、站点管理和版本下载

HTTP 和 WebSocket 共用同一套 JSON 命令，实时事件通过 WebSocket 推送，文件使用 HTTPS 传输

## 选择接入方式

| 客户端 | 登录 | 业务命令 | 文件 |
| --- | --- | --- | --- |
| 浏览器 | `POST /v2/session`，由 API 主机签发会话 Cookie | 同一 API 主机的 `/v2/socket`，浏览器在握手时带上符合条件的 Cookie | 同一 API 主机的 `/v2/media/{media_id}` |
| 原生 App、桌面 App、CLI | `account.signin` 或 `account.signup`，从响应读取 `token` | `/v2/socket` 或 `POST /v2/call`，使用 Bearer；也可以在匿名 WebSocket 上调用 `session.attach` | `/v2/media/{media_id}`，私有对象使用 Bearer |

| 入口 | HTTPS 基址 | WebSocket |
| --- | --- | --- |
| CsAC 站点 | `https://csac.chat` | `wss://csac.chat/v2/socket` |
| 直连入口 | `https://ws.csac.chat:8443` | `wss://ws.csac.chat:8443/v2/socket` |

自托管时将主机和端口换成实例配置；要指定 HTTP/2 或 HTTP/3 建立 WebSocket，请选择支持 Extended CONNECT 的客户端和入口，见[握手与连接](api/protocol.md#websocket)

## 接入顺序

1. 按客户端类型完成登录、恢复已有会话，或引导注册及邮箱验证
2. 连接 WebSocket，协商 `csac.v2`，读取 `hello`，再加载账号资料和初始列表
3. 根据命令的访问级别和对象权限显示操作，为要求 `key` 的写入保存重试键与原始参数
4. 本地保存已应用的持久事件游标，重连后先回放，再接续实时推送
5. 用 `media.prepare` 获取上传地址，将上传完成的 `media_id` 关联到消息、头像、动态或群文件

[快速接入与可运行示例](api/quickstart.md)从登录、会话列表、发消息演示到回放和上传

## 文档索引

| 章节 | 内容 |
| --- | --- |
| [协议、鉴权与错误](api/protocol.md) | 请求和响应、权限、幂等、Origin、WebSocket 握手、关闭码 |
| [事件推送与断线回放](api/events.md) | 事件目录、负载结构、同步流程、游标与去重 |
| [账号与邮箱验证](api/accounts.md) | 注册、登录、验证、个人资料、会话管理、注销与恢复 |
| [会话和消息](api/chat.md) | 会话列表、历史、回复、@、拍一拍、精华、撤回、已读、正在输入、在线状态 |
| [好友与通知](api/friends-notices.md) | 好友申请、备注、屏蔽、删除与找回、通知列表与已读 |
| [群组](api/groups.md) | 创建与发现、加群、成员、权限、经验与等级、转让、站点管理入口 |
| [媒体与群文件](api/media.md) | 上传、鉴权下载、静态资源、群文件与目录 |
| [空间](api/space.md) | 动态、时间线、点赞、评论与楼中楼 |
| [站点管理与举报](api/administration.md) | 用户及群组封禁、注册码、举报提交与审核 |
| [版本与下载](api/releases.md) | 查询客户端版本、平台资源下载 |

## HTTP 路由速查

| 方法与路径 | 用途 | 返回 |
| --- | --- | --- |
| `POST /v2/call` | 业务命令，包括 `system.operations` | JSON 响应封套 |
| `POST /v2/session` | 浏览器登录、注册、恢复、Cookie 检查与注销 | JSON 响应封套，登录及注销设置 `Set-Cookie` |
| `GET /v2/socket` 或 Extended `CONNECT /v2/socket` | 实时命令与推送 | `csac.v2` WebSocket |
| `PUT /v2/media/{media_id}` | 上传经 `media.prepare` 申请的对象 | 媒体 JSON 或媒体错误 |
| `GET` / `HEAD /v2/media/{media_id}` | 读取对象字节或元数据响应头 | 媒体字节、Range 响应或媒体错误 |
| `GET` / `HEAD /v2/assets/default-avatar.svg` | 默认头像 | SVG |
| `GET` / `HEAD /v2/media/builtin-stickers/{code}` | 内置表情图片，从 `stickers.list` 读取 URL | 图片 |
| `GET` / `HEAD /v2/releases/{version}/{platform}` | 发布文件 | 安装包或其他发布资源 |
| `GET` / `HEAD /healthz` | 进程探活 | `200`，GET 返回 `{"status":"ok"}` |
| `GET` / `HEAD /readyz` | PostgreSQL 就绪检查 | `200 ready` 或 `503 unavailable`，GET 返回 `{"status":"ready"}` 或 `{"status":"unavailable"}` |

表中的 `GET` / `HEAD` 共用同一路径，`HEAD` 不返回正文；跨源 `OPTIONS` 预检见[浏览器规则](api/protocol.md#origin-与-cors)

## 查询实例支持的操作

```sh
curl -fsS https://csac.chat/v2/call \
  -H 'Content-Type: application/json' \
  --data '{"id":"ops-1","op":"system.operations","data":{}}'
```

响应封套的 `data` 是操作数组，每项给出 `name`、`access`、`writes`、`requires_key`，仅限 WebSocket 的命令另有 `websocket_only: true`

`writes` 表示使用写事务，`requires_key` 表示必须发送幂等键；对象级权限、参数和返回值见对应章节；`/v2/session` 独有的 `session.current` 见[浏览器会话](api/protocol.md#浏览器会话)

本手册记录现有 CsAC v2 客户端接口；服务运维入口从[后端 README](../README.md)查阅，Bot 和小程序不在本版客户端接口中
