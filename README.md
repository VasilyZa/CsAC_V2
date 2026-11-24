# CsAC V2

![Go 1.26+](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)
![PostgreSQL 18](https://img.shields.io/badge/PostgreSQL-18-4169E1?logo=postgresql&logoColor=white)
![HTTP/1.1 · 2 · 3](https://img.shields.io/badge/HTTP-1.1%20%7C%202%20%7C%203-334155)
[![Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-2E7D32)](LICENSE)

CsAC 的 Go 后端，提供即时消息、好友、群组、空间动态和站点管理

[在线站点](https://csac.chat/) · [CsAC Web](https://github.com/VasilyZa/CsAC_Web) · [部署](#部署)

## 功能

- 私聊与群聊、消息回执、在线状态、断线补收和群成员经验值
- 邮箱验证、账号恢复、Cookie 会话与多名站点管理员授权
- 流式媒体传输、群文件和内置表情
- PostgreSQL 持久化，使用 WebSocket 处理实时命令与事件

## 部署

复制环境模板，并把生成的值依次填入 `.env`

```sh
cp .env.example .env
openssl rand -hex 32      # CSAC_POSTGRES_PASSWORD
openssl rand -base64 32   # CSAC_PASSWORD_PEPPER
openssl rand -base64 32   # CSAC_ACCOUNT_KEY
```

将 `tls.crt` 和 `tls.key` 放进 `CSAC_CERT_DIR`，授予容器 UID `65532` 读取权限，然后启动服务

```sh
docker compose up -d --build
```

Compose 默认监听本机的 `8443` 端口，对外部署时设置 `CSAC_BIND_IP` 并开放该端口的 TCP 和 UDP

| 配置项 | 用途 |
| --- | --- |
| `CSAC_SMTP_*` | 发送邮箱验证码和账号恢复邮件 |
| `CSAC_ADMIN_UIDS` | 逗号分隔的现有账号 UID，修改后重建服务容器 |
| `CSAC_ALLOWED_ORIGINS` | 允许访问服务的 Web Origin |
| `CSAC_CERT_DIR` | TLS 证书目录，默认 `./certs` |

<details>
<summary>构建二进制和迁移工具</summary>

```sh
mkdir -p bin
go build -trimpath -o bin/csacd ./cmd/csacd
go build -trimpath -tags timetzdata -o bin/csac-migrate ./cmd/csac-migrate
```

直接运行 `csacd` 时设置 `GODEBUG=http2xconnect=1`，并配置 PostgreSQL、TLS 和应用密钥

`bin/csac-migrate -h` 列出旧数据的检查和导入命令

</details>

## 接口

[第三方客户端 API 开发者手册](docs/api.md)

| 路径 | 用途 |
| --- | --- |
| `/v2/socket` | WebSocket 命令、事件和补收 |
| `/v2/session` | 浏览器登录、会话检查和退出 |
| `/v2/call` | HTTP 命令 |
| `/v2/media/` | 文件上传与下载 |
| `/healthz`、`/readyz` | 存活检查和数据库就绪检查 |

调用 `system.operations` 可获取命令目录

WebSocket 子协议为 `csac.v2`

内置表情的原始许可随资源保存在 [`internal/stickerart`](internal/stickerart/LICENSE)

## 许可证

[Apache License 2.0](LICENSE)
