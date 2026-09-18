"use strict";

const { test } = require("node:test");
const assert = require("node:assert/strict");
const http = require("node:http");
const { once } = require("node:events");
const { BrowserRecorder, SegmentTranscriber, LocalSpeaker } = require("./audio-ui.cjs");
const { startLiveHelper } = require("./live-helper.cjs");
const { startAudioHost } = require("./audio-host.cjs");
const { buildUserScript } = require("./build-user-script.cjs");
const vm = require("node:vm");
const key = "SYNTHETIC_ONLY_GATEWAY_KEY_456789";
const sdp = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n";
const tick = () => new Promise(resolve => setImmediate(resolve));
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }
async function create(helper) {
  const response = await fetch(helper.connection.helperUrl, { method: "POST",
    headers: { Authorization: `Bearer ${helper.connection.localCapability}`, "Content-Type": "application/json" },
    body: JSON.stringify({ sdp, session: { model: "fixture-model" } }) });
  await response.text(); assert.equal(response.status, 201);
}
function connect(helper) {
  return new Promise((resolve, reject) => {
    const request = http.request(helper.sidebandBaseUrl.replace("ws:", "http:") + "/live/rtc_test", { headers: {
      Authorization: `Bearer ${key}`, Connection: "Upgrade", Upgrade: "websocket",
      "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version": "13",
    } });
    request.on("response", response => { response.resume(); resolve(response.statusCode); });
    request.on("upgrade", (_response, socket) => { socket.destroy(); reject(new Error("Unexpected upgrade")); });
    request.on("error", reject); request.end();
  });
}
const validCreated = () => new Response(sdp, { status: 201, headers: { Location: "/v1/live/rtc_test", "Content-Type": "application/sdp" } });

test("Sideband失败保留401而非重置连接，同步异常/伪造握手释放租约", async t => {
  for (const mode of ["unauthorized", "throw", "bad-handshake"]) {
    const upstream = http.createServer();
    upstream.on("upgrade", (_req, socket) => {
      socket.on("error", () => {});
      if (mode === "unauthorized") socket.end("HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
      else socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: INVALID\r\n\r\n");
    });
    upstream.listen(0, "127.0.0.1"); await once(upstream, "listening");
    t.after(() => upstream.close());
    const helper = await startLiveHelper({ gatewayOrigin: "https://fixture.invalid", apiKey: key, fetchImpl: validCreated,
      requestImpl: (url, options) => {
        if (mode === "throw") throw new Error("SYNTHETIC_SENSITIVE_DETAIL");
        return http.request({ host: "127.0.0.1", port: upstream.address().port, path: url.pathname, ...options });
      } });
    t.after(() => helper.close());
    await create(helper);
    assert.equal(await connect(helper), mode === "unauthorized" ? 401 : 502);
    assert.equal(helper.diagnostics().calls, 0);
    await helper.close();
  }
});

test("控制端鉴权和Origin隔离，按需能力不含网关Key，停机释放三个端口", async () => {
  let upstreamCalls = 0;
  const host = await startAudioHost({ gatewayOrigin: "https://fixture.invalid", apiKey: key,
    fetchImpl: async () => { upstreamCalls++; return validCreated(); } });
  const headers = { Authorization: `Bearer ${host.bootstrap}` };
  assert.equal((await fetch(host.controlUrl + "/audio/session", { method: "POST" })).status, 401);
  assert.equal((await fetch(host.controlUrl + "/audio/session", { method: "POST", headers: { ...headers, Origin: "https://evil.invalid" } })).status, 403);
  const response = await fetch(host.controlUrl + "/audio/session", { method: "POST", headers });
  assert.equal(response.status, 200);
  const connection = await response.json();
  assert.ok(!JSON.stringify(connection).includes(key));
  assert.match(connection.live.helperUrl, /^http:\/\/127.0.0.1:/);
  assert.equal(upstreamCalls, 0);
  const status = await fetch(host.controlUrl + "/audio/status", { headers });
  assert.equal((await status.json()).ready, true);
  await host.close(); await host.close();
  for (const target of [host.controlUrl, connection.live.helperUrl, connection.dictation.helperUrl]) {
    await assert.rejects(fetch(target, { signal: AbortSignal.timeout(1000) }));
  }
});

test("脚本无网关Key，未知构建停用，重新加载清理前次安装", async () => {
  const script = buildUserScript({ controlUrl: "http://127.0.0.1:19846", bootstrap: "x".repeat(43), destination: "https://fixture.invalid" });
  assert.ok(!script.includes(key));
  let disposals = 0;
  const window = { __concordAudio: { dispose: () => disposals++ }, removeEventListener() {} };
  window.top = window;
  const sandbox = { window, location: { protocol: "app:", hostname: "-", href: "app://-/index.html" },
    document: { querySelectorAll: () => [] }, performance: { getEntriesByType: () => [] },
    console: { warn() {} }, URL };
  vm.runInNewContext(script, sandbox);
  await tick();
  assert.equal(disposals, 1);
  assert.equal(window.__concordAudio.status, "failed");
  assert.match(window.__concordAudio.error, /构建尚未核验/);
});

test("录音在权限返回前取消，不创建音频上下文，不保留麦克风", async () => {
  const mic = deferred();
  let stopped = 0, contexts = 0;
  const runtime = {
    navigator: { mediaDevices: { getUserMedia: () => mic.promise } },
    AudioContext: class { constructor() { contexts++; } },
    clearTimeout, setTimeout,
  };
  const recorder = new BrowserRecorder(() => {}, () => {}, () => {}, runtime);
  const starting = recorder.start();
  await recorder.stop(true);
  mic.resolve({ getTracks: () => [{ stop: () => stopped++ }] });
  await starting;
  assert.equal(stopped, 1); assert.equal(contexts, 0);
});

test("resume中取消后不复活计时器，自动停止会通知UI且释放音轨", async () => {
  for (const cancelled of [true, false]) {
    const resumed = deferred();
    let stopped = 0, timers = 0, callback, notified = 0, finished = 0;
    const node = () => ({ connect() {}, disconnect() {}, gain: { value: 1 } });
    const runtime = {
      navigator: { mediaDevices: { getUserMedia: async () => ({ getTracks: () => [{ stop: () => stopped++ }] }) } },
      AudioContext: class {
        sampleRate = 24000;
        createMediaStreamSource = node; createGain = node; createScriptProcessor = node;
        resume = () => resumed.promise; close = async () => {};
      },
      setTimeout(fn) { timers++; callback = fn; return 1; }, clearTimeout() {},
    };
    const recorder = new BrowserRecorder(() => ({ cancel() {}, finish: async () => finished++ }), assert.fail, () => notified++, runtime);
    const starting = recorder.start(); await tick();
    if (cancelled) await recorder.stop(true);
    resumed.resolve(); await starting;
    if (!cancelled) { callback(); await tick(); }
    assert.equal(timers, cancelled ? 0 : 1);
    assert.equal(stopped, 1); assert.equal(notified, 1); assert.equal(finished, cancelled ? 0 : 1);
  }
});

test("队列饱和停止而不无限占内存，上传错误不泄漏未处理Promise", async () => {
  const work = deferred(); let errors = 0, texts = 0;
  const transcriber = new SegmentTranscriber({ sampleRate: 8000, segmentSeconds: 1, maxQueued: 1,
    upload: () => work.promise, onText: () => texts++, onError: () => errors++ });
  transcriber.push(new Float32Array(8000)); await tick();
  assert.throws(() => transcriber.push(new Float32Array(8000)), /排队/);
  work.resolve("晚到文本");
  await assert.rejects(transcriber.finish(), /排队/);
  assert.equal(errors, 1); assert.equal(texts, 0); assert.equal(transcriber.buffer.length, 0);
  assert.equal(transcriber.pending, 0);
  assert.throws(() => new LocalSpeaker({ getVoices: () => [{ localService: false }], cancel() {} }, class {}).speak("测试"), /本机语音/);
});

test("未启用Live时完全保留原生调用，空闲朗读卸载不影响其他队列", () => {
  const { installAudioAdapter } = require("./desktop-audio-adapter.cjs");
  const options = {method:"POST",body:"native"};
  const client = {fetch:(url,init)=>({url,init})};
  const dispose = installAudioAdapter(client,()=>assert.fail("不应读取音频配置"),{liveEnabled:false});
  assert.equal(client.fetch("/wham/realtime/calls?intent=quicksilver&architecture=avas",options).init,options);
  dispose();
  let cancelled=0;
  const speaker=new LocalSpeaker({getVoices:()=>[{localService:true,lang:"zh-CN"}],speaking:true,cancel:()=>cancelled++},class{});
  speaker.cancel();
  assert.equal(cancelled,0);
  assert.throws(()=>speaker.speak("测试"),/已有其他朗读/);
});

test("Windows朗读取消、分段和对象URL释放，不干扰浏览器全局语音队列", async()=>{
  const {WindowsSpeaker}=require("./windows-speaker.cjs");
  const posted=[],revoked=[];
  class AudioFixture {
    play(){queueMicrotask(()=>this.onended?.());return Promise.resolve();}
    pause(){} removeAttribute(){} load(){}
  }
  const speaker=new WindowsSpeaker({
    fetch:async(_url,init)=>{posted.push(JSON.parse(init.body).text);return new Response(new Uint8Array([1,2]),{headers:{"Content-Type":"audio/wav"}});},
    connection:async()=>({url:"http://127.0.0.1:19846",capability:"synthetic"}),
    Audio:AudioFixture,urls:{createObjectURL:()=>`blob:${posted.length}`,revokeObjectURL:x=>revoked.push(x)},onError:assert.fail,
  });
  speaker.speak("字".repeat(801));await speaker.work;
  assert.deepEqual(posted.map(x=>x.length),[400,400,1]);
  assert.equal(revoked.length,3);
  const waiting=deferred();
  speaker.connection=()=>waiting.promise;
  speaker.speak("取消测试");const work=speaker.work;speaker.cancel();
  waiting.resolve({url:"http://127.0.0.1:19846",capability:"synthetic"});await work;
  assert.equal(posted.length,3);
});

test("本机合成端点仍要求能力且并发有界，无上游网络请求",async()=>{
  const synthesis=deferred();let calls=0;
  const host=await startAudioHost({gatewayOrigin:"https://fixture.invalid",apiKey:key,
    speechImpl:async()=>{calls++;return synthesis.promise;},
    fetchImpl:()=>assert.fail("朗读不得访问网关")});
  try{
    const make=()=>fetch(host.controlUrl+"/audio/speech",{method:"POST",headers:{Authorization:"Bearer "+host.bootstrap,"Content-Type":"application/json"},body:JSON.stringify({text:"测试"})});
    assert.equal((await fetch(host.controlUrl+"/audio/speech",{method:"POST",body:"{}"})).status,401);
    const first=make();
    for(let i=0;i<20&&!calls;i++)await new Promise(r=>setTimeout(r,5));
    assert.equal((await make()).status,429);
    synthesis.resolve(Buffer.from("RIFFfixture"));
    const response=await first;
    assert.equal(response.status,200);assert.equal(response.headers.get("content-type"),"audio/wav");
    await response.arrayBuffer();assert.equal(calls,1);
  }finally{await host.close();}
});
