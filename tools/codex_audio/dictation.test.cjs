"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const http = require("node:http");
const { fork } = require("node:child_process");
const path = require("node:path");
const { once } = require("node:events");
const { startDictationHelper } = require("./dictation-helper.cjs");
const { installDictationAdapter, rewrite, matchesDictation } = require("./dictation-adapter.cjs");

// 所有音频和凭据均为合成夹具；真实 loopback HTTP，网关 fetch 为受控替身。
const mime = "multipart/form-data; boundary=test_audio";
const audio = Buffer.concat([
  Buffer.from('--test_audio\r\nContent-Disposition: form-data; name="file"; filename="test.wav"\r\nContent-Type: audio/wav\r\n\r\n'),
  Buffer.from([0, 1, 128, 255, 13, 10, 0]),
  Buffer.from("\r\n--test_audio--\r\n"),
]);
const gatewayKey = "SYNTHETIC_GATEWAY_CREDENTIAL";
const response = text => new Response(JSON.stringify({ text }), { headers: { "Content-Type": "application/json" } });

async function setup(t, options = {}) {
  const calls = [];
  const helper = await startDictationHelper({
    gatewayOrigin: "https://gateway.invalid", apiKey: gatewayKey,
    fetchImpl: async (url, init) => { calls.push({ url, init }); return response("这是合成转录结果"); },
    ...options,
  });
  t.after(() => helper.close());
  return { ...helper, calls };
}
function post(helper, overrides = {}) {
  return fetch(helper.connection.helperUrl, {
    method: "POST", body: audio,
    headers: { "Content-Type": mime, Authorization: `Bearer ${helper.connection.localCapability}` },
    ...overrides,
  });
}
async function errorCode(result, status, code) {
  assert.equal(result.status, status);
  assert.deepEqual(await result.json(), { error: { code } });
}
function rawPost(helper, { path, headers = {}, chunks = [audio] } = {}) {
  return new Promise((resolve, reject) => {
    const url = new URL(helper.connection.helperUrl);
    const req = http.request(url, {
      method: "POST", path: path || url.pathname,
      headers: { "Content-Type": mime, Authorization: `Bearer ${helper.connection.localCapability}`, ...headers },
    }, res => {
      const parts = [];
      res.on("data", part => parts.push(part));
      res.on("end", () => resolve({ status: res.statusCode, body: JSON.parse(Buffer.concat(parts)) }));
    });
    req.on("error", reject);
    for (const chunk of chunks) req.write(chunk);
    req.end();
  });
}

test("原生形状上传经过独立认证，音频字节和 boundary 不变", async t => {
  const helper = await setup(t);
  const client = {
    fetch(url, options) { assert.equal(this, client); return fetch(url, options); },
  };
  const original = client.fetch;
  const uninstall = installDictationAdapter(client, helper.connection);
  const result = await client.fetch("/transcribe", {
    method: "POST", body: audio,
    headers: {
      "Content-Type": mime, Authorization: "Bearer OFFICIAL_AUTH",
      Cookie: "OFFICIAL_COOKIE", "x-oai-attestation": "PROOF",
      "X-OpenAI-Attach-Auth": "1", "X-OpenAI-Attach-Integrity-State": "1",
    },
  });
  assert.deepEqual(await result.json(), { text: "这是合成转录结果" });
  assert.equal(helper.calls.length, 1);
  const { url, init } = helper.calls[0];
  assert.equal(url, "https://gateway.invalid/transcribe");
  assert.deepEqual(init.body, audio);
  assert.deepEqual(init.headers, {
    Authorization: `Bearer ${gatewayKey}`, "Content-Type": mime, Accept: "application/json",
  });
  assert.equal(init.redirect, "manual");
  assert.equal(uninstall(), true);
  assert.equal(client.fetch, original);
});

test("非目标、关闭、GET、流式及相似地址保持原样", () => {
  const options = { method: "POST", body: audio };
  for (const url of ["/responses", "/dictation/stream", "/transcribe?x=1",
    "https://elsewhere.invalid/backend-api/transcribe", "https://chatgpt.com/backend-api/transcribe?x=1"]) {
    assert.equal(matchesDictation(url, options), false);
    assert.deepEqual(rewrite(url, options, { enabled: true }), { url, options });
  }
  assert.equal(matchesDictation("/transcribe", { method: "GET" }), false);
  assert.equal(matchesDictation("https://chatgpt.com/backend-api/transcribe", options), true);
  assert.equal(rewrite("/transcribe", options, { enabled: false }).options, options);
  const sentinel = {};
  const client = { fetch(_url, received) { assert.equal(received, options); return sentinel; } };
  const restore = installDictationAdapter(client, {});
  assert.equal(client.fetch("/responses", options), sentinel);
  assert.equal(restore(), true);
});

test("重复安装不叠加请求，卸载恢复继承的方法", () => {
  const prototype = { fetch() { return Promise.resolve(response("")); } };
  const client = Object.create(prototype);
  const restore = installDictationAdapter(client, {});
  assert.throws(() => installDictationAdapter(client, {}), /already adapted/);
  assert.equal(restore(), true);
  assert.equal(Object.hasOwn(client, "fetch"), false);
  assert.equal(client.fetch, prototype.fetch);
});

test("卸载不覆盖另一个插件的后续改动，残留包装器停用选路", () => {
  const client = { fetch(url) { return url; } };
  const restore = installDictationAdapter(client, {});
  const wrapped = client.fetch;
  const other = function (url, options) { return wrapped.call(this, url, options); };
  client.fetch = other;
  assert.equal(restore(), false);
  assert.equal(client.fetch, other);
  assert.equal(client.fetch("/transcribe", { method: "POST" }), "/transcribe");
});

test("拒绝匿名、错误能力、跨 Origin、伪造 Host 和带查询参数的请求", async t => {
  const helper = await setup(t);
  await errorCode(await post(helper, { headers: { "Content-Type": mime } }), 401, "audio_capability_required");
  await errorCode(await post(helper, {
    headers: { "Content-Type": mime, Authorization: "Bearer WRONG" },
  }), 401, "audio_capability_required");
  const foreign = await rawPost(helper, { headers: { Origin: "https://evil.invalid" } });
  assert.equal(foreign.status, 403);
  assert.equal((await rawPost(helper, { headers: { Host: "evil.invalid" } })).status, 403);
  assert.equal((await rawPost(helper, { path: "/dictation/transcribe?url=https://evil.invalid" })).status, 404);
  assert.equal(helper.calls.length, 0);
});

test("CORS 仅允许显式来源且不会公开能力令牌", async t => {
  const helper = await setup(t, { allowedOrigins: ["https://desktop.invalid"] });
  const headers = {
    Origin: "https://desktop.invalid", "Access-Control-Request-Method": "POST",
    "Access-Control-Request-Headers": "content-type,authorization",
  };
  const result = await fetch(helper.connection.helperUrl, { method: "OPTIONS", headers });
  assert.equal(result.status, 204);
  assert.equal(result.headers.get("Access-Control-Allow-Origin"), "https://desktop.invalid");
  assert.equal(await result.text(), "");
  const rejected = await fetch(helper.connection.helperUrl, {
    method: "OPTIONS", headers: { ...headers, "Access-Control-Request-Headers": "x-unsafe" },
  });
  assert.equal(rejected.status, 403);
  assert.equal(helper.calls.length, 0);
});

test("能力过期拒绝新上传", async t => {
  const helper = await setup(t, { capabilityTtlMs: 20 });
  await new Promise(resolve => setTimeout(resolve, 35));
  await errorCode(await post(helper), 401, "audio_capability_expired");
  assert.equal(helper.calls.length, 0);
});

test("已知长度与分块上传均受上限约束", async t => {
  const helper = await setup(t, { uploadLimit: 32 });
  await errorCode(await post(helper), 413, "audio_upload_too_large");
  const chunked = await rawPost(helper, { chunks: [audio.subarray(0, 10), audio.subarray(10)] });
  assert.equal(chunked.status, 413);
  assert.equal(helper.calls.length, 0);
});

test("无效类型、压缩上传和空体在上游前被拒绝", async t => {
  const helper = await setup(t);
  const auth = { Authorization: `Bearer ${helper.connection.localCapability}` };
  await errorCode(await post(helper, { headers: { ...auth, "Content-Type": "text/plain" } }), 415, "audio_multipart_required");
  await errorCode(await post(helper, {
    headers: { ...auth, "Content-Type": mime, "Content-Encoding": "gzip" },
  }), 415, "audio_multipart_required");
  await errorCode(await post(helper, { body: Buffer.alloc(0) }), 400, "audio_upload_empty");
  assert.equal(helper.calls.length, 0);
});

for (const status of [401, 403, 413, 422, 429, 500]) {
  test(`网关 ${status} 保持错误且不重传录音`, async t => {
    let count = 0;
    const helper = await setup(t, {
      fetchImpl: async () => { count++; return new Response("PRIVATE upstream body", { status }); },
    });
    await errorCode(await post(helper), status, "audio_gateway_rejected");
    assert.equal(count, 1);
  });
}

test("禁止重定向、HTML、坏 JSON、缺 text 以及超大响应", async t => {
  const cases = [
    [() => new Response("", { status: 307, headers: { Location: "https://evil.invalid" } }), "audio_gateway_rejected"],
    [() => new Response("<html/>", { headers: { "Content-Type": "text/html" } }), "audio_gateway_rejected"],
    [() => new Response("{", { headers: { "Content-Type": "application/json" } }), "audio_gateway_invalid_json"],
    [() => new Response(Buffer.from([123, 34, 116, 101, 120, 116, 34, 58, 34, 255, 34, 125]),
      { headers: { "Content-Type": "application/json" } }), "audio_gateway_invalid_json"],
    [() => new Response('{"other":1}', { headers: { "Content-Type": "application/json" } }), "audio_gateway_missing_text"],
    [() => response("x".repeat(65)), "audio_gateway_response_too_large"],
  ];
  for (const [fetchImpl, code] of cases) {
    const helper = await setup(t, { responseLimit: 64, fetchImpl });
    await errorCode(await post(helper), 502, code);
    await helper.close();
  }
});

test("空转录成功，附加上游字段不会泄漏", async t => {
  const helper = await setup(t, {
    fetchImpl: async () => new Response('{"text":"","debug":"PRIVATE"}', { headers: { "Content-Type": "application/json" } }),
  });
  const result = await post(helper);
  assert.equal(result.status, 200);
  assert.deepEqual(await result.json(), { text: "" });
});

test("网关异常详情不回显", async t => {
  const helper = await setup(t, { fetchImpl: async () => { throw { status: 401, code: gatewayKey }; } });
  await errorCode(await post(helper), 502, "audio_gateway_unavailable");
});

test("超时传到上游并保持 504", async t => {
  let aborted = false;
  const helper = await setup(t, { timeoutMs: 40, fetchImpl: (_url, { signal }) => new Promise((_resolve, reject) => {
    signal.addEventListener("abort", () => { aborted = true; reject(signal.reason); }, { once: true });
  }) });
  await errorCode(await post(helper), 504, "audio_timeout");
  assert.equal(aborted, true);
});

test("下游中途取消后中止上游", async t => {
  let notifyStarted, notifyAborted;
  const started = new Promise(resolve => { notifyStarted = resolve; });
  const aborted = new Promise(resolve => { notifyAborted = resolve; });
  const helper = await setup(t, { fetchImpl: (_url, { signal }) => new Promise((_resolve, reject) => {
    signal.addEventListener("abort", () => { notifyAborted(); reject(signal.reason); }, { once: true });
    notifyStarted();
  }) });
  const controller = new AbortController();
  const upload = post(helper, { signal: controller.signal });
  const rejected = assert.rejects(upload, { name: "AbortError" });
  await started;
  controller.abort();
  await rejected;
  await aborted;
});

test("取消前置检查不上传，卸载中止正在进行的听写", async t => {
  const helper = await setup(t);
  const controller = new AbortController();
  controller.abort();
  const client = { fetch: (url, options) => fetch(url, options) };
  const restore = installDictationAdapter(client, helper.connection);
  await assert.rejects(client.fetch("/transcribe", { method: "POST", body: audio, signal: controller.signal }), { name: "AbortError" });
  assert.equal(helper.calls.length, 0);
  assert.equal(restore(), true);
  let invoked;
  const ready = new Promise(resolve => { invoked = resolve; });
  const stalled = { fetch(_url, { signal }) {
    invoked();
    return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(signal.reason), { once: true }));
  } };
  const stop = installDictationAdapter(stalled, helper.connection);
  const promise = stalled.fetch("/transcribe", { method: "POST", body: audio, headers: { "Content-Type": mime } });
  const rejected = assert.rejects(promise, { name: "AbortError" });
  await ready;
  assert.equal(stop(), true);
  await rejected;
});

test("两个在途上传后返回 429，结束后可重新使用", async t => {
  const releases = [];
  let calls = 0;
  let notifyFull;
  const full = new Promise(resolve => { notifyFull = resolve; });
  const helper = await setup(t, { fetchImpl: () => {
    if (++calls > 2) return response("继续可用");
    return new Promise(resolve => {
      releases.push(() => resolve(response("")));
      if (releases.length === 2) notifyFull();
    });
  } });
  const first = post(helper);
  const second = post(helper);
  await full;
  await errorCode(await post(helper), 429, "audio_helper_busy");
  releases.splice(0).forEach(release => release());
  assert.equal((await first).status, 200);
  assert.equal((await second).status, 200);
  assert.equal(releases.length, 0);
  assert.deepEqual(await (await post(helper)).json(), { text: "继续可用" });
});

test("上传未结束即断开，释放槽位且不进入网关", async t => {
  const helper = await setup(t);
  await new Promise(resolve => {
    const req = http.request(helper.connection.helperUrl, {
      method: "POST",
      headers: { "Content-Type": mime, Authorization: `Bearer ${helper.connection.localCapability}` },
    });
    req.on("error", () => {});
    req.write(audio.subarray(0, 10), () => setTimeout(() => { req.destroy(); resolve(); }, 10));
  });
  await new Promise(resolve => setTimeout(resolve, 10));
  assert.equal(helper.calls.length, 0);
  assert.equal((await post(helper)).status, 200);
});

test("读取响应正文同样受总超时约束", async t => {
  let cancelled = false;
  const helper = await setup(t, {
    timeoutMs: 40,
    fetchImpl: async () => new Response(new ReadableStream({
      start(stream) { stream.enqueue(Buffer.from('{"text":"')); },
      cancel() { cancelled = true; },
    }), { headers: { "Content-Type": "application/json" } }),
  });
  await errorCode(await post(helper), 504, "audio_timeout");
  assert.equal(cancelled, true);
});

test("停用后端口释放，连接配置不包含网关 Key", async t => {
  const helper = await setup(t);
  assert.equal(JSON.stringify(helper.connection).includes(gatewayKey), false);
  await helper.close();
  await helper.close();
  await assert.rejects(post(helper), /fetch failed/);
});

test("不接受非 HTTPS 目标、带路径目标、非法 Key 与宽泛 Origin", async () => {
  for (const gatewayOrigin of ["http://gateway.invalid", "https://gateway.invalid/v1", "https://u:p@gateway.invalid", "https://gateway.invalid/?x=1"]) {
    await assert.rejects(startDictationHelper({ gatewayOrigin, apiKey: gatewayKey }));
  }
  await assert.rejects(startDictationHelper({ gatewayOrigin: "https://gateway.invalid", apiKey: "x\r\ny" }));
  for (const origin of ["*", "null", "https://desktop.invalid/path"]) {
    await assert.rejects(startDictationHelper({ gatewayOrigin: "https://gateway.invalid", apiKey: gatewayKey, allowedOrigins: [origin] }));
  }
});

test("适配器拒绝错误目标、无能力令牌、过期与控制字符", () => {
  const config = { enabled: true, helperUrl: "http://127.0.0.1:17889/dictation/transcribe", localCapability: "SYNTHETIC_LOCAL_CAPABILITY_ONLY" };
  const options = { method: "POST", body: audio, headers: { "Content-Type": mime } };
  for (const bad of [{ helperUrl: "https://remote.invalid/dictation/transcribe" }, { localCapability: "" }, { expiresAt: 1 }]) {
    assert.throws(() => rewrite("/transcribe", options, { ...config, ...bad }));
  }
  for (const headers of [
    { "Content-Type": `${mime}\r\nx: 1` },
    { "Content-Type": mime, "content-type": mime },
  ]) assert.throws(() => rewrite("/transcribe", { ...options, headers }, config));
  const mapped = rewrite("/transcribe", options, config);
  assert.equal(mapped.options.body, audio);
  assert.equal(mapped.options.credentials, "omit");
  assert.equal(mapped.options.redirect, "error");
});

test("可信 IPC 启动、连接配置交付与停机不向标准输出泄漏凭据", async t => {
  const child = fork(path.join(__dirname, "helper-process.cjs"), [], { silent: true });
  t.after(() => { if (child.connected) child.disconnect(); });
  let output = "";
  child.stdout.on("data", data => { output += data; });
  child.stderr.on("data", data => { output += data; });
  const ready = once(child, "message");
  const exited = once(child, "exit");
  child.send({ type: "start", gatewayOrigin: "https://gateway.invalid", apiKey: gatewayKey });
  const [message] = await ready;
  assert.equal(message.type, "ready");
  assert.equal(JSON.stringify(message).includes(gatewayKey), false);
  assert.equal((await fetch(message.connection.helperUrl, { method: "POST" })).status, 401);
  child.send({ type: "stop" });
  assert.deepEqual(await exited, [0, null]);
  assert.equal(output, "");
  await assert.rejects(fetch(message.connection.helperUrl), /fetch failed/);
});
