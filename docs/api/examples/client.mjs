// Node.js 24+，使用内置 fetch、ws 8 和系统信任的 TLS 证书
import { strict as assert } from 'node:assert';
import { randomUUID } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { basename } from 'node:path';
import WebSocket from 'ws';

const base = new URL(process.env.CSAC_BASE ?? 'https://csac.chat');
assert.equal(base.protocol, 'https:');
const socketURL = new URL('/v2/socket', base);
socketURL.protocol = 'wss:';
const conversationID = process.env.CSAC_CONVERSATION_ID ? Number(process.env.CSAC_CONVERSATION_ID) : null;
assert(conversationID === null || (Number.isSafeInteger(conversationID) && conversationID > 0),
  'CSAC_CONVERSATION_ID 必须是正整数');
if (process.env.CSAC_FILE) assert(conversationID !== null, '发送文件需要 CSAC_CONVERSATION_ID');

class RPCError extends Error {
  constructor({ code, message }) {
    super(`${code}: ${message}`);
    this.code = code;
  }
}

function unwrap(reply) {
  if (reply.error) throw new RPCError(reply.error);
  if (reply.data?.error === 'invalid_email_challenge') {
    throw new RPCError({ code: reply.data.error, message: '验证码或恢复令牌已失效' });
  }
  assert(Object.hasOwn(reply, 'data'), '响应缺少 data');
  return reply.data;
}

function envelope(op, data, key) {
  return { id: randomUUID(), op, ...(key ? { key } : {}), data };
}

async function callHTTP(op, data = {}, { token, key } = {}) {
  const request = envelope(op, data, key);
  const r = await fetch(new URL('/v2/call', base), {
    method: 'POST', redirect: 'error', signal: AbortSignal.timeout(15000),
    headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
    body: JSON.stringify(request),
  });
  const reply = await r.json();
  if (reply.error) throw new RPCError(reply.error);
  assert(r.ok && reply.id === request.id, `HTTP 命令失败：${r.status}`);
  return unwrap(reply);
}

// 通过 HTTP/1.1 升级，在 hello 后显式绑定 Bearer
async function openSocket(token, buffered) {
  const ws = new WebSocket(socketURL, 'csac.v2', { handshakeTimeout: 8000, perMessageDeflate: false });
  const pending = new Map();
  let hello;
  const close = () => {
    if (ws.readyState !== WebSocket.CLOSING && ws.readyState !== WebSocket.CLOSED) ws.close();
  };
  const finished = new Promise((resolve) => ws.addEventListener('close', resolve, { once: true }));
  try {
    hello = await new Promise((resolve, reject) => {
      const helloTimer = setTimeout(() => reject(new Error('等待 hello 超时')), 8000);
      ws.addEventListener('error', () => {
        clearTimeout(helloTimer);
        reject(new Error('WebSocket 连接失败'));
      }, { once: true });
      ws.addEventListener('close', ({ code }) => {
        clearTimeout(helloTimer);
        const error = new Error(`WebSocket 已关闭：${code}`);
        reject(error);
        for (const job of pending.values()) {
          clearTimeout(job.timer);
          job.reject(error);
        }
        pending.clear();
      }, { once: true });
      ws.addEventListener('message', ({ data }) => {
        const frame = JSON.parse(data);
        if (frame.type === 'hello') {
          clearTimeout(helloTimer);
          resolve(frame);
        } else if (frame.type === 'event') {
          if (frame.seq !== undefined) buffered.push(frame);
        } else {
          const job = pending.get(frame.id);
          if (!job) return;
          clearTimeout(job.timer);
          pending.delete(frame.id);
          try { job.resolve(unwrap(frame)); } catch (error) { job.reject(error); }
        }
      });
    });
    assert.equal(ws.protocol, 'csac.v2');
    assert.equal(hello.version, 2);
    function call(op, data = {}, key) {
      const request = envelope(op, data, key);
      return new Promise((resolve, reject) => {
        if (ws.readyState !== WebSocket.OPEN) return reject(new Error('WebSocket 未连接'));
        const timer = setTimeout(() => {
          pending.delete(request.id);
          reject(new Error(`${op} 超时，按原 key 查询结果`));
        }, 15000);
        pending.set(request.id, { resolve, reject, timer });
        try { ws.send(JSON.stringify(request)); } catch (error) {
          clearTimeout(timer);
          pending.delete(request.id);
          reject(error);
        }
      });
    }
    return { identity: await call('session.attach', { token }), call, close, finished };
  } catch (error) {
    close();
    throw error;
  }
}

async function replay(socket, start, onReset) {
  let cursor = start;
  const events = new Map();
  for (;;) {
    const page = await socket.call('events.replay', { after: cursor, limit: 100 });
    if (page.reset) {
      events.clear();
      await onReset();
      cursor = page.head;
      continue;
    }
    for (const event of page.events) events.set(event.seq, event);
    cursor = page.next;
    if (!page.has_more) return { cursor, events };
  }
}

async function uploadAndSend(socket, token, path) {
  const bytes = await readFile(path);
  const mime = process.env.CSAC_FILE_MIME ?? 'application/octet-stream';
  const upload = await socket.call('media.prepare', {
    purpose: 'message', mime, filename: basename(path), size: bytes.length, as_file: true,
  }, randomUUID());
  const uploadURL = new URL(upload.upload_path, base);
  assert.equal(uploadURL.origin, base.origin);
  const r = await fetch(uploadURL, {
    method: 'PUT', redirect: 'error', signal: AbortSignal.timeout(60000),
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': mime }, body: bytes,
  });
  const media = await r.json();
  if (media.error) throw new RPCError(media.error);
  assert.equal(r.status, 201);
  assert.equal(media.id, upload.id);
  const metadata = await socket.call('media.info', { id: media.id });
  assert.equal(metadata.size, bytes.length);
  const transfer = new URL(media.download_path, base);
  assert.equal(transfer.origin, base.origin);
  const download = await fetch(transfer, {
    redirect: 'error', signal: AbortSignal.timeout(15000),
    headers: { Authorization: `Bearer ${token}`, Range: 'bytes=0-0' },
  });
  assert.equal(download.status, 206);
  assert.equal(new Uint8Array(await download.arrayBuffer())[0], bytes[0]);
  const fileMessage = await socket.call('chat.send', {
    conversation_id: conversationID, kind: 'file', attachment_id: media.id,
  }, randomUUID());
  console.log('文件消息序号', fileMessage.seq);
}

async function main() {
  let token = process.env.CSAC_TOKEN;
  if (!token) {
    const { CSAC_USERNAME: username, CSAC_PASSWORD: password } = process.env;
    assert(username && password, '提供 CSAC_TOKEN 或 CSAC_USERNAME 和 CSAC_PASSWORD');
    ({ token } = await callHTTP('account.signin', { username, password }));
  }
  const self = await callHTTP('account.self', {}, { token });
  const first = await openSocket(token, []);
  let start, sent;
  try {
    assert.equal(first.identity.user_id, self.user_id);
    assert(first.identity.chat_enabled, '请先完成邮箱验证，或使用有聊天资格的账号');
    start = process.env.CSAC_EVENT_AFTER ?? first.identity.head;
    const list = await first.call('conversations.list', { limit: 10 });
    console.log('当前账号', self.user_id, '会话页条数', list.conversations.length);
    if (conversationID !== null) {
      await first.call('chat.history', { conversation_id: conversationID, limit: 10 });
      const data = { conversation_id: conversationID, kind: 'text', text: '**第三方客户端** 已连接' };
      const key = randomUUID();
      sent = await first.call('chat.send', data, key);
      const status = await callHTTP('commands.status', { key }, { token });
      assert.equal(status.state, 'complete');
      assert.equal(status.result.seq, sent.seq);
      console.log('文字消息序号', sent.seq);
      if (process.env.CSAC_FILE) await uploadAndSend(first, token, process.env.CSAC_FILE);
    }
  } finally {
    first.close();
    await first.finished;
  }

  const buffered = [];
  const next = await openSocket(token, buffered);
  try {
    const { cursor, events } = await replay(next, start, async () => {
      await next.call('conversations.list', { limit: 10 });
      if (conversationID !== null) await next.call('chat.history', { conversation_id: conversationID, limit: 10 });
    });
    for (const event of buffered) {
      // 回放区间采用回放结果，实时事件只补扫描边界之外的尾部
      if (BigInt(event.seq) > BigInt(cursor)) events.set(event.seq, event);
    }
    const batch = [...events.values()].sort((a, b) => BigInt(a.seq) < BigInt(b.seq) ? -1 : 1);
    const appliedThrough = batch.at(-1)?.seq ?? cursor;
    const through = BigInt(appliedThrough) > BigInt(cursor) ? appliedThrough : cursor;
    await next.call('events.confirm', { through });
    console.log('本次回放与实时增量', batch.length, '游标', through);
    if (sent) {
      const visible = await next.call('chat.message.get', { conversation_id: conversationID, seq: sent.seq });
      assert.equal(visible.seq, sent.seq);
      console.log('重连后读取到消息', visible.seq);
    }
  } finally {
    next.close();
    await next.finished;
  }
}

main().catch((error) => {
  console.error(error.message);
  process.exitCode = 1;
});
