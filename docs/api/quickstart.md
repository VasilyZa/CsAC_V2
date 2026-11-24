# 快速接入

[返回 API 手册](../api.md) · [完整协议](protocol.md) · [事件同步](events.md) · [可运行 Node.js 示例](examples/client.mjs)

下面从已取得聊天权限的账号开始；没有账号时，走[注册或邮箱验证](accounts.md#注册与登录)；示例中的 `https://csac.chat` 可换为自托管实例的 HTTPS 基址；发消息、点赞和文件上传首先使用测试账号与测试会话

## 原生或 CLI：获取会话并查询

发起密码登录，从 `data.token` 读取令牌；这个方式适合原生 App、服务工具和 CLI；浏览器 Cookie 登录见下一节

```sh
API=https://csac.chat
# 在本地环境提供 CSAC_USERNAME 和 CSAC_PASSWORD
LOGIN=$(jq -nc --arg u "$CSAC_USERNAME" --arg p "$CSAC_PASSWORD" \
  '{id:"sign-1",op:"account.signin",data:{username:$u,password:$p}}')
TOKEN=$(curl -fsS "$API/v2/call" -H 'Content-Type: application/json' \
  --data "$LOGIN" | jq -er '.data.token')

curl -fsS "$API/v2/call" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  --data '{"id":"conv-1","op":"conversations.list","data":{"limit":20}}'
```

业务 WebSocket 的 URL 是 `wss://csac.chat/v2/socket`，子协议传 `csac.v2`；支持自定义请求头的原生库可在握手带 `Authorization: Bearer …`；否则用匿名握手，收到 `hello` 后发送 `session.attach`：

```json
{"id":"attach-1","op":"session.attach","data":{"token":"example-session-token-from-server"}}
```

收到带 `user_id`、`chat_enabled` 和 `head` 的响应后发应用命令；先用 `conversations.list` 获取会话 ID，也可对已经互为好友的目标调用 `chat.direct.open`；群入口从 `groups.list` 选群，或从 `groups.discover` 查公开群后完成加入

### 从好友申请到私聊

按表中身份分别发送命令，所有写入各用一个 `key`；业务对象 ID 取自前一步响应，不用请求 `id` 或全局事件序号代替

| 谁执行 | 命令和请求 `data` | 如何继续 |
| --- | --- | --- |
| 发起方 | `account.lookup`，`{"username":"peer"}` | 读取对方 `user_id`，示例为 `18` |
| 发起方 | `friends.request`，`{"user_id":18,"message":"你好"}` | 记下返回的 `request_id`，示例为 `307` |
| 接收方 | `friends.requests`，`{"direction":"incoming","status":"pending"}` | 在列表中定位要接受的申请 |
| 接收方 | `friends.respond`，`{"request_id":307,"accept":true}` | 响应 `status: "accepted"` 后双方成为好友 |
| 任一方 | `chat.direct.open`，`{"user_id":18}`，填该设备的对方 UID | 从 `data.conversation_id` 得到之后发消息用的会话 ID |

公开审批群用类似的状态链：成员执行 `groups.join` 得到 `request_id`，群管理者从 `groups.join_requests` 定位申请后执行 `groups.respond_join`，申请者在通知收到结果，再刷新 `groups.list` 与 `conversations.list`；公开直入群的 `groups.join` 会直接返回 `joined`

## 浏览器：Cookie 登录与连接

从与 API 同源的页面或已配置白名单的同站点前端运行；`fetch` 会自动带上浏览器的 `Origin`；通过标准 `WebSocket` 构造器建立连接，浏览器将按 Cookie 策略处理凭据；下面用 `/v2/session` 登录、恢复和登出，由 `onFrame` 接收后续业务帧

```js
// 同源页面可用 location.origin，前后端分离时指定同站点的 API 地址
const api = location.origin;

async function browserSession(op, data = {}) {
  const id = crypto.randomUUID();
  const response = await fetch(new URL('/v2/session', api), {
    method: 'POST',
    credentials: 'include',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id, op, data }),
  });
  const reply = await response.json();
  if (reply.error) throw Object.assign(new Error(reply.error.message), { code: reply.error.code });
  if (!response.ok || reply.id !== id) throw new Error('会话响应不匹配');
  return reply.data;
}

async function connectAs(userID, onFrame) {
  const url = new URL('/v2/socket', api);
  url.protocol = 'wss:';
  const socket = new WebSocket(url, 'csac.v2');
  return new Promise((resolve, reject) => {
    socket.addEventListener('error', () => reject(new Error('连接失败')), { once: true });
    socket.addEventListener('close', ({ code }) => reject(new Error(`连接关闭：${code}`)), { once: true });
    socket.addEventListener('message', ({ data }) => {
      const frame = JSON.parse(data);
      if (frame.type === 'hello') {
        if (frame.user_id !== userID || socket.protocol !== 'csac.v2' || frame.version !== 2) {
          socket.close();
          reject(new Error('会话不匹配，请重新登录'));
        } else {
          resolve({ socket, hello: frame });
        }
      } else {
        onFrame(frame); // 使用响应 id 和推送 type 分流
      }
    });
  });
}

// username 和 password 由自己的登录表单提供
async function signIn(username, password, onFrame) {
  const { user_id } = await browserSession('account.signin', { username, password });
  return connectAs(user_id, onFrame);
}

async function restoreSession(onFrame) {
  const { user_id } = await browserSession('session.current');
  return connectAs(user_id, onFrame);
}

async function signOut(socket) {
  await browserSession('account.logout');
  socket.close();
}
```

若 `session.current` 返回 `unauthenticated`，显示登录界面；`hello.chat_enabled` 为 `false` 时可发 `account.self` 并进入邮箱绑定流程；持久化登录由 Cookie 完成，不在 `localStorage` 复制凭据；没有通过 CORS 和同站点条件的外部网页可以改用原生客户端，或为自托管 API 配置同站点前端

## 发送文字并对账

在已绑定会话的 WebSocket 发送：

```json
{"id":"chat-1","op":"chat.send","key":"5e369e05-e7f6-454a-8e30-2909c1980cd9","data":{"conversation_id":42,"kind":"text","text":"你好，我是第三方客户端"}}
```

成功响应的 `data.seq` 是消息序号字符串；用 `chat.message.get {conversation_id,seq}` 或 `message.created` 获取详情；请求结果未知时，发 `commands.status {key}`；仍不确定时，用原 `key` 和原参数重试，生成新的请求 `id`

加载聊天页用 `chat.history {conversation_id,limit:30}`；翻向更早时传该页最旧消息的 `seq` 给 `before`；用户看到消息后，用一个新写入键发送 `chat.read {conversation_id,seq}`，`seq` 取已浏览到的位置

## 断线接续

新连接的 `hello` 或 `session.attach` 返回 `head`；若客户端已有游标，用旧游标开始 `events.replay`，同时缓冲推送；否则先按 [首次连接和重连](events.md#首次连接和重连)加载快照，再补回放

```json
{"id":"resume-1","op":"events.replay","data":{"after":"180","limit":100}}
```

接续时将 `next` 用作下一页 `after`，一直读到 `has_more: false`；按全局序号合并事件，稳定排序并去重；已回放区间采用回放可见性，随后接超过最终 `next` 的实时事件；应用到本地后保存游标；例如 `message.created` 的 `seq` 用于事件去重，`data.seq` 用于会话内消息去重

## 上传并发送文件

先创建一个示例文件和上传预约；`as_file: true` 表示通用消息附件，由服务端按文件保留期管理

```sh
printf 'hello\n' > hello.txt
PREPARE=$(curl -fsS "$API/v2/call" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  --data "$(jq -nc --arg key "$(uuidgen)" --argjson n "$(wc -c < hello.txt)" \
    '{id:"prepare-1",op:"media.prepare",key:$key,data:{purpose:"message",mime:"text/plain",filename:"hello.txt",size:$n,as_file:true}}')")
UPLOAD_PATH=$(printf '%s' "$PREPARE" | jq -er '.data.upload_path')
MEDIA=$(curl -fsS -X PUT "$API$UPLOAD_PATH" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: text/plain' --data-binary @hello.txt)
MEDIA_ID=$(printf '%s' "$MEDIA" | jq -er '.id')
```

PUT 返回 `201` 和媒体对象；通过 WebSocket 发送 `chat.send`，选择 `kind: "file"`，并将 `MEDIA_ID` 放到 `attachment_id`；接收方从消息取得该 UUID，再用 `media.info` 读取文件信息和 `download_path`；以下演示用 HTTP 发同一条命令，并从原地址下载：

```sh
# CSAC_CONVERSATION_ID 为数字形式的测试会话 ID
curl -fsS "$API/v2/call" -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  --data "$(jq -nc --arg key "$(uuidgen)" --arg media "$MEDIA_ID" --argjson room "$CSAC_CONVERSATION_ID" \
    '{id:"file-1",op:"chat.send",key:$key,data:{conversation_id:$room,kind:"file",attachment_id:$media}}')"
DOWNLOAD_PATH=$(printf '%s' "$MEDIA" | jq -er '.download_path')
curl -fsS "$API$DOWNLOAD_PATH" -H "Authorization: Bearer $TOKEN" -o received.txt
```

用浏览器上传时保持同一流程，`fetch(uploadPath, {method:'PUT',credentials:'include',headers:{'Content-Type':mime},body:file})`，`file` 为浏览器 `File`；用 `file.size` 而不是字符串长度作为预约字节数

## 运行 Node.js 示例

将 [client.mjs](examples/client.mjs) 保存到本地的示例目录，在该目录使用 Node.js 24 或以上运行：

```sh
npm install ws@8
export CSAC_BASE=https://csac.chat
export CSAC_USERNAME=your_username
# 在当前 shell 的环境中提供 CSAC_PASSWORD，或改用 CSAC_TOKEN
node client.mjs
```

示例通过 HTTP 登录、用 `ws` 库通过 HTTP/1.1 建立 WebSocket 并显式绑定、读取会话、断开并连接一次，最后调用事件回放与确认；只设置登录信息时读取数据；再设置 `CSAC_CONVERSATION_ID` 会发送一条 Markdown 文字，可同时设置 `CSAC_FILE=/path/to/hello.txt` 和 `CSAC_FILE_MIME=text/plain`，演示预约、上传、Range 下载和关联到新消息；用 `CSAC_EVENT_AFTER` 提供之前保存的游标，可以从该处重放；自托管测试使用私有 CA 时，将 CA 传给 Node.js 的 `NODE_EXTRA_CA_CERTS`，保留 TLS 验证；需要通过 HTTP/2 或 HTTP/3 建立 WebSocket 时，按[握手表](protocol.md#websocket)选择支持 Extended CONNECT 的客户端库
