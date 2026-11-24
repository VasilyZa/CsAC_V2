# 版本查询与下载

[返回 API 手册](../api.md) · [HTTP 路由速查](../api.md#http-路由速查)

客户端先查询当前已发布版本，再从 `assets` 选择自己的安装包，使用服务端给出的 `download` 下载；查询和下载都允许匿名访问

| 命令 | 权限 | `key` | 请求 `data` | 成功 `data` |
| --- | --- | --- | --- | --- |
| `release.latest` | `public` | — | `{installed_version?}`，字符串 | `{available,release?}` |

- 有发布时，`release` 始终描述实例的当前发布版本，即使 `available: false`
- 不传 `installed_version` 或传空字符串时，只要有发布就返回 `available: true`；有当前版本参数时，仅在它可以比较且所发布的版本更新时返回 `true`
- 没有发布时为 `{available:false}`，省略 `release`；版本可用不等于当前平台有安装包，继续查 `assets` 的键

版本比较使用点分隔数字核心和可选的 `-rN` 数字修订号，允许头部 `v` 或 `V`；按数字逐段比较，缺失段按 `0` 对待；同一核心中正式版排在全部修订版之后，例如 `2.4.0-r3 < 2.4.0-r10 < 2.4.0`；无法识别的非空已安装版本得到 `available: false`

```json
{"id":"release-1","op":"release.latest","data":{"installed_version":"2.3.0"}}
```

```json
{"id":"release-1","data":{"available":true,"release":{"version":"2.4.0","notes":"新增媒体下载与断线回放","published_at":"2026-01-20T08:30:00Z","assets":{"android":{"filename":"csac-android.apk","download":"/v2/releases/2.4.0/android","sha256":"0000000000000000000000000000000000000000000000000000000000000000","bytes":4321098}}}}}
```

`release` 含 `version`、`notes`、`published_at` 和 `assets`；`assets` 是以平台标识为键的对象，平台随发布包而变化；每项包含：

| 字段 | 含义 |
| --- | --- |
| `filename` | 建议保存的文件名 |
| `download` | 相对 API HTTPS 主机的下载路径 `/v2/releases/{version}/{platform}`，路径末段是平台键而不是文件名 |
| `sha256` | 文件 SHA-256 小写十六进制字符串，示例用占位值 |
| `bytes` | 文件字节数 |

`GET` 下载字节，`HEAD` 查询响应头；下载路由返回 `application/octet-stream`、带文件名的 `Content-Disposition`，以及以 SHA-256 为值的 `ETag`；支持 HTTP Range 和条件请求；下载完成后根据 `bytes` 和 `sha256` 检查完整性，再交给系统安装；路径不可用时可能返回 `404`，重新调用 `release.latest` 获取当前资源
