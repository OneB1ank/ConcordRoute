"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const http = require("node:http");
const { once } = require("node:events");

// 所有输入与密钥均为测试夹具；真实上游验收由单独脚本显式执行。
const key = "SYNTHETIC_GATEWAY_KEY_123456";
const sdp = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n";
const livePath = "/wham/realtime/calls?intent=quicksilver&architecture=avas";

test("Live 只改选路，保留 session、真实关联字段与取消信号", async () => {
  const { installAudioAdapter } = require("./desktop-audio-adapter.cjs");
  const calls = [];
  const client = { async fetch(url, init) { calls.push({ url, init }); return new Response(sdp, { status: 201, headers: { Location: "/v1/live/rtc_test" } }); } };
  const original = client.fetch;
  const signal = new AbortController().signal;
  const body = JSON.stringify({ sdp, session: { model: "gpt-live-1-codex", delegation: { type: "client" }, initial_items: [], audio: { output: { voice: "marin" } } } });
  const uninstall = installAudioAdapter(client, async () => ({
    live: { helperUrl: "http://127.0.0.1:17890/audio/live", localCapability: "synthetic_capability_123456789", expiresAt: Date.now() + 10000 },
  }));
  await client.fetch(livePath, { method: "POST", body, signal, headers: {
    "Content-Type": "application/json", "OpenAI-Alpha": "quicksilver=v2",
    "Thread-Id": "thread-native", "Session-Id": "session-native",
    "X-OpenAI-Attach-Auth": "1", Cookie: "official-secret", Authorization: "Bearer official-secret",
  } });
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, "http://127.0.0.1:17890/audio/live");
  assert.equal(calls[0].init.body, body);
  assert.equal(calls[0].init.headers["thread-id"], "thread-native");
  assert.equal(calls[0].init.headers.cookie, undefined);
  assert.ok(!JSON.stringify(calls[0].init.headers).includes("official-secret"));
  assert.equal(uninstall(), true);
  assert.equal(client.fetch, original);
});

test("非音频请求不经过 helper、不额外获取配置，重复安装被阻止", async () => {
  const { installAudioAdapter } = require("./desktop-audio-adapter.cjs");
  let configurations = 0;
  const body = { method: "POST", body: "UNCHANGED" };
  const client = { fetch: (url, init) => ({ url, init }) };
  const uninstall = installAudioAdapter(client, async () => { configurations++; return {}; });
  assert.throws(() => installAudioAdapter(client, async () => ({})));
  const result = client.fetch("/responses", body);
  assert.equal(result.init, body);
  assert.equal(configurations, 0);
  uninstall();
});

test("Live 创建必须为201及同源合法callId，错误不伪装成功", async t => {
  const { startLiveHelper } = require("./live-helper.cjs");
  for (const [status, location, type, accepted] of [
    [201, "/v1/live/rtc_valid", "application/sdp", true],
    [200, "/v1/live/rtc_valid", "application/sdp", false],
    [201, "https://other.invalid/v1/live/rtc_valid", "application/sdp", false],
    [201, "/v1/live/not-a-call", "application/sdp", false],
    [201, "/v1/live/rtc_valid?token=secret", "application/sdp", false],
    [201, "/v1/live/rtc_valid", "text/html", false],
  ]) {
    const helper = await startLiveHelper({ gatewayOrigin: "https://fixture.invalid", apiKey: key,
      fetchImpl: async () => new Response(sdp, { status, headers: { Location: location, "Content-Type": type } }) });
    t.after(() => helper.close());
    const response = await fetch(helper.connection.helperUrl, { method: "POST",
      headers: { Authorization: `Bearer ${helper.connection.localCapability}`, "Content-Type": "application/json" },
      body: JSON.stringify({ sdp, session: { model: "gpt-live-1-codex" } }) });
    assert.equal(response.status, accepted ? 201 : 502);
    if (accepted) {
      assert.equal(response.headers.get("location"), "/v1/live/rtc_valid");
      assert.equal(await response.text(), sdp);
    } else assert.ok(!(await response.text()).includes("secret"));
  }
});

test("Live 创建与Sideband锁定同一Key/callId并清理socket", async t => {
  const { startLiveHelper } = require("./live-helper.cjs");
  let upgradeHeaders, upgradePath, upgradeCount = 0;
  const upstream = http.createServer();
  upstream.on("upgrade", (req, socket) => {
    upgradeHeaders = req.headers; upgradePath = req.url; upgradeCount++;
    socket.write("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n");
    socket.on("data", data => socket.write(data));
    socket.on("error", () => {});
  });
  upstream.listen(0, "127.0.0.1"); await once(upstream, "listening");
  t.after(() => { upstream.closeAllConnections(); upstream.close(); });
  const helper = await startLiveHelper({
    gatewayOrigin: "https://fixture.invalid", apiKey: key,
    fetchImpl: async (_url, init) => {
      assert.equal(init.headers.Authorization, `Bearer ${key}`);
      return new Response(sdp, { status: 201, headers: { Location: "/v1/live/rtc_bound", "Content-Type": "application/sdp" } });
    },
    requestImpl: (url, options) => {
      assert.equal(url.href, "https://fixture.invalid/v1/live/rtc_bound");
      return http.request({ host: "127.0.0.1", port: upstream.address().port, path: url.pathname, ...options });
    },
  });
  t.after(() => helper.close());
  const response = await fetch(helper.connection.helperUrl, { method: "POST",
    headers: { Authorization: `Bearer ${helper.connection.localCapability}`, "Content-Type": "application/json" },
    body: JSON.stringify({ sdp, session: { model: "gpt-live-1-codex" } }) });
  assert.equal(response.status, 201); await response.text();
  const sideband = new URL(helper.sidebandBaseUrl.replace("ws:", "http:") + "/live/rtc_bound");
  function connect(bearer, suffix = "") {
    return new Promise((resolve, reject) => {
      const req = http.request(sideband.href + suffix, { headers: {
        Authorization: `Bearer ${bearer}`, Connection: "Upgrade", Upgrade: "websocket",
        "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version": "13",
      } });
      req.on("upgrade", (res, socket) => resolve({ status: res.statusCode, socket }));
      req.on("response", res => { res.resume(); resolve({ status: res.statusCode }); });
      req.on("error", reject); req.end();
    });
  }
  assert.equal((await connect("SYNTHETIC_OTHER_KEY")).status, 401);
  assert.equal((await connect(key, "?call=other")).status, 404);
  assert.equal(upgradeCount, 0);
  const opened = await connect(key);
  assert.equal(opened.status, 101);
  assert.equal(upgradeHeaders.authorization, `Bearer ${key}`);
  assert.equal(upgradePath, "/v1/live/rtc_bound");
  opened.socket.destroy();
  await helper.close();
  assert.equal(helper.diagnostics().calls, 0);
});

test("分段转文字有界、保持顺序、停止排空尾段，取消后不回填", async () => {
  const { SegmentTranscriber, pcm16Wav } = require("./audio-ui.cjs");
  const texts = [], uploads = [];
  const transcriber = new SegmentTranscriber({
    sampleRate: 24000, segmentSeconds: 1,
    upload: async (wav, signal) => { uploads.push(wav); signal.throwIfAborted(); return `片段${uploads.length}`; },
    onText: text => texts.push(text),
  });
  transcriber.push(new Float32Array(24000));
  transcriber.push(new Float32Array(12000));
  await transcriber.finish();
  assert.deepEqual(texts, ["片段1", "片段2"]);
  assert.equal(uploads[0].byteLength, 44 + 24000 * 2);
  assert.equal(new TextDecoder().decode(pcm16Wav(new Float32Array(100), 24000).slice(0, 4)), "RIFF");
  assert.throws(() => transcriber.push(new Float32Array(1)), /closed/);
  let resolveUpload;
  const cancelledTexts = [];
  const cancelled = new SegmentTranscriber({ sampleRate: 24000, segmentSeconds: 1,
    upload: () => new Promise(resolve => { resolveUpload = resolve; }), onText: text => cancelledTexts.push(text) });
  cancelled.push(new Float32Array(24000));
  await new Promise(resolve => setImmediate(resolve));
  cancelled.cancel();
  resolveUpload("迟到文本");
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(cancelledTexts, []);
});

test("朗读只选本机声音、取消释放队列，不请求远端服务", async () => {
  const { LocalSpeaker } = require("./audio-ui.cjs");
  let spoken, cancelled = 0;
  const voices = [{ name: "Remote", lang: "zh-CN", localService: false }, { name: "Local", lang: "zh-CN", localService: true }];
  const speaker = new LocalSpeaker({ getVoices: () => voices, speak: value => { spoken = value; }, cancel: () => cancelled++ },
    class { constructor(text) { this.text = text; } });
  speaker.speak("这是本机朗读测试");
  assert.equal(spoken.voice.name, "Local");
  assert.equal(spoken.text, "这是本机朗读测试");
  speaker.cancel();
  assert.ok(cancelled > 0);
});
