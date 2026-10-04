import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import OpenAI from 'openai';
import { OpenAIRealtimeWS } from 'openai/realtime/ws';

assert.ok(Number(process.versions.node.split('.')[0]) >= 22, 'SDK 测试要求 Node >=22');
for (const [name, version] of [['openai', '7.27.0'], ['ws', '8.21.0']]) {
  const pkg = JSON.parse(await readFile(new URL(`node_modules/${name}/package.json`, import.meta.url)));
  assert.equal(pkg.version, version, '依赖必须来自已锁定安装');
}
let input = '';
for await (const chunk of process.stdin) input += chunk;
const config = JSON.parse(input);
const endpoint = new URL(config.baseURL);
assert.equal(endpoint.protocol, 'https:');
assert.equal(endpoint.hostname, 'localhost');
assert.equal(endpoint.pathname, '/v1');
assert.ok(['roundtrip', 'untrusted', 'heartbeat', 'no-pong'].includes(config.mode));

const rt = new OpenAIRealtimeWS({
  model: 'real-model',
  options: {
    ca: config.ca,
    rejectUnauthorized: true,
    autoPong: config.mode !== 'no-pong',
    headers: {
      'OpenAI-Safety-Identifier': 'synthetic-safety',
      'OpenAI-Organization': 'synthetic-org',
      'OpenAI-Project': 'synthetic-project',
      Origin: 'https://synthetic.example',
      Cookie: 'synthetic-cookie=value',
      'X-Private': 'synthetic-private',
    },
  },
}, new OpenAI({ apiKey: 'sk-test-1234567890', baseURL: config.baseURL }));
assert.equal(rt.url.protocol, 'wss:');
assert.equal(rt.url.pathname, '/v1/realtime');
assert.equal(rt.url.search, '?model=real-model');

const result = { mode: config.mode, opened: false, rejectedTLS: false, received: 0, sent: 0, pings: 0 };
const rawMessages = [], sdkEvents = [], sdkErrors = [], socketErrors = [];
let closed = false, failure, wake, authorized = false, pipelined = false, upgrade, openedAt;
const notify = () => { wake?.(); wake = undefined; };
const waitFor = async (predicate) => {
  while (!predicate()) {
    if (failure) throw failure;
    assert.ok(!closed, '收到所需事件前连接已关闭');
    await new Promise((resolve) => { wake = resolve; });
  }
};
const deadline = setTimeout(() => {
  failure = new Error('SDK 验收超过 10 秒，强制释放 socket');
  rt.socket.terminate();
  notify();
}, 10_000);

rt.on('event', (event) => { sdkEvents.push(event); });
rt.on('error', (error) => { sdkErrors.push(error); notify(); });
rt.socket.on('error', (error) => { socketErrors.push(error); notify(); });
rt.socket.on('upgrade', (response) => { upgrade = response; });
rt.socket.on('ping', () => { result.pings++; notify(); });
// 比 SDK JSON 解析更早取得 bytes，防止结构等价掩盖空白/未知字段/消息类型的损失。
rt.socket.prependListener('message', (data, isBinary) => {
  rawMessages.push({ data: Buffer.from(data), isBinary });
  notify();
});
rt.socket.on('close', (code, reason) => {
  closed = true;
  result.closeCode = code;
  result.closeReason = reason.toString();
  result.openMillis = openedAt === undefined ? 0 : performance.now() - openedAt;
  notify();
});
rt.socket.on('open', () => {
  result.opened = true;
  openedAt = performance.now();
  authorized = rt.socket._socket.authorized === true;
  if (config.mode === 'roundtrip') {
    // 101 后立即发送，不等 session.created；首事件缓冲不能吞掉提前 pipeline 的 update。
    pipelined = rawMessages.length === 0 && sdkEvents.length === 0;
    rt.send(JSON.parse(config.steps[1].raw));
    result.sent++;
  }
  notify();
});

try {
  await waitFor(() => result.opened || closed);
  if (config.mode === 'untrusted') {
    assert.equal(result.opened, false, '不可信 CA 意外通过 TLS');
    const allowed = ['UNABLE_TO_VERIFY_LEAF_SIGNATURE', 'UNABLE_TO_GET_ISSUER_CERT_LOCALLY', 'CERT_SIGNATURE_FAILURE'];
    assert.equal(socketErrors.length, 1);
    assert.ok(allowed.includes(socketErrors[0].code), '失败不是证书信任拒绝');
    assert.equal(sdkErrors.length, 1);
    assert.equal(sdkErrors[0].cause?.code, socketErrors[0].code);
    assert.equal(rawMessages.length, 0);
    result.rejectedTLS = true;
    result.tlsCode = socketErrors[0].code;
  } else {
    assert.equal(authorized, true, 'TLS socket 未完成 CA 与主机名校验');
    assert.equal(upgrade.statusCode, 101);
    assert.equal(upgrade.headers['sec-websocket-extensions'], undefined);
    assert.equal(upgrade.headers['sec-websocket-protocol'], undefined);
    if (config.mode === 'roundtrip') assert.equal(pipelined, true);
    for (const [index, step] of config.steps.entries()) {
      if (step.direction === 'send') {
        if (index === 1 && config.mode === 'roundtrip') continue;
        rt.send(JSON.parse(step.raw));
        result.sent++;
      } else {
        await waitFor(() => rawMessages.length > result.received);
        const raw = rawMessages[result.received];
        assert.equal(raw.isBinary, false, `第 ${index} 条消息类型改变`);
        assert.deepEqual(raw.data, Buffer.from(step.raw), `第 ${index} 条原字节改变`);
        assert.deepEqual(sdkEvents[result.received], JSON.parse(step.raw), 'SDK 事件分发丢失');
        result.received++;
      }
    }
    if (config.mode === 'heartbeat') {
      // 六次真实 ping 跨过网关 HTTP total；业务保持沉默，不能补造 response.done。
      await waitFor(() => result.pings >= 6);
    }
    if (config.mode !== 'no-pong') rt.close({ code: 1000, reason: 'sdk-close' });
    await waitFor(() => closed);
    assert.equal(rawMessages.length, result.received, '多出未预期的应用消息');
    assert.equal(sdkEvents.length, result.received);
    assert.equal(socketErrors.length, 0);
    if (config.mode === 'roundtrip') {
      assert.equal(sdkErrors.length, 1, '带内错误必须分发一次且不妨碍后续请求');
      assert.equal(sdkErrors[0].error?.code, 'invalid_value');
      assert.equal(sdkErrors[0].event_id, 'e-error');
    } else {
      assert.equal(sdkErrors.length, 0);
    }
  }
  if (failure) throw failure;
  console.log(JSON.stringify(result));
} finally {
  clearTimeout(deadline);
  // 不用 process.exit 掩盖残留句柄；失败也要终止连接并等原生 close。
  if (!closed) {
    const stopped = new Promise((resolve) => rt.socket.once('close', resolve));
    rt.socket.terminate();
    await stopped;
  }
}
