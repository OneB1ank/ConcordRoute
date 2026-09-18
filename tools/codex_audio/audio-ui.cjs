"use strict";

// PCM 以真实采样率封装为独立 WAV，每段均可单独解码；不切割压缩容器伪造音频段。
function pcm16Wav(samples, sampleRate) {
  if (!(samples instanceof Float32Array) || !Number.isInteger(sampleRate) || sampleRate < 8000 || sampleRate > 96000
      || samples.length > sampleRate * 10) throw new Error("Invalid PCM segment");
  const bytes = new Uint8Array(44 + samples.length * 2), view = new DataView(bytes.buffer);
  function text(offset, value) { for (let i = 0; i < value.length; i++) bytes[offset + i] = value.charCodeAt(i); }
  text(0, "RIFF"); view.setUint32(4, bytes.length - 8, true); text(8, "WAVE"); text(12, "fmt ");
  view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, sampleRate, true); view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true); view.setUint16(34, 16, true); text(36, "data");
  view.setUint32(40, samples.length * 2, true);
  for (let i = 0; i < samples.length; i++) {
    const value = Number.isFinite(samples[i]) ? Math.max(-1, Math.min(1, samples[i])) : 0;
    view.setInt16(44 + i * 2, Math.round(value * (value < 0 ? 32768 : 32767)), true);
  }
  return bytes;
}

class SegmentTranscriber {
  constructor({ sampleRate, segmentSeconds = 4, maxQueued = 4, upload, onText, onError = () => {} }) {
    if (!Number.isInteger(sampleRate) || sampleRate < 8000 || sampleRate > 96000
        || !Number.isInteger(segmentSeconds) || segmentSeconds < 1 || segmentSeconds > 10
        || !Number.isInteger(maxQueued) || maxQueued < 1 || maxQueued > 8) throw new Error("Invalid segment limit");
    this.sampleRate = sampleRate; this.limit = sampleRate * segmentSeconds; this.maxQueued = maxQueued;
    this.buffer = new Float32Array(this.limit); this.used = 0; this.pending = 0;
    this.upload = upload; this.onText = onText; this.onError = onError;
    this.controller = new AbortController(); this.work = Promise.resolve();
    this.closed = false; this.error = null;
  }
  push(samples) {
    if (this.closed) throw new Error("Transcriber closed");
    if (!(samples instanceof Float32Array) || samples.length > this.sampleRate * 10) throw new Error("Invalid audio frame");
    let offset = 0;
    while (offset < samples.length) {
      const length = Math.min(samples.length - offset, this.limit - this.used);
      this.buffer.set(samples.subarray(offset, offset + length), this.used);
      this.used += length; offset += length;
      if (this.used === this.limit) this.flush();
    }
  }
  flush() {
    if (!this.used) return;
    if (this.pending >= this.maxQueued) {
      this.error = new Error("转录处理落后，已停止录音以限制排队；已有文字保留");
      this.cancel(); this.onError(this.error); throw this.error;
    }
    const wav = pcm16Wav(this.buffer.subarray(0, this.used), this.sampleRate);
    this.used = 0; this.pending++;
    this.work = this.work.then(async () => {
      const signal = this.controller.signal;
      signal.throwIfAborted();
      const text = await this.upload(wav, signal);
      signal.throwIfAborted();
      if (typeof text !== "string") throw new Error("Invalid transcript");
      if (text) this.onText(text);
    }).catch(error => {
      if (!this.controller.signal.aborted) {
        this.error = error; this.cancel(); this.onError(error);
      }
    }).finally(() => { this.pending--; });
  }
  async finish() {
    if (!this.closed) { this.flush(); this.closed = true; }
    await this.work;
    this.buffer = new Float32Array(0);
    if (this.error) throw this.error;
  }
  cancel() { this.closed = true; this.used = 0; this.controller.abort(); this.buffer = new Float32Array(0); }
}

class LocalSpeaker {
  constructor(synthesis = globalThis.speechSynthesis, Utterance = globalThis.SpeechSynthesisUtterance) {
    this.synthesis = synthesis; this.Utterance = Utterance;
  }
  speak(text) {
    if (!this.synthesis || !this.Utterance) throw new Error("当前客户端没有本机朗读接口");
    if (typeof text !== "string" || !text.trim() || text.length > 20000) throw new Error("请选择或输入 1–20000 字的朗读内容");
    const local = this.synthesis.getVoices().filter(voice => voice.localService);
    const voice = local.find(voice => /^zh[-_]/i.test(voice.lang)) || local[0];
    if (!voice) throw new Error("本机语音尚未就绪或没有已安装声音");
    if (!this.current && (this.synthesis.speaking || this.synthesis.pending)) throw new Error("已有其他朗读，请等待结束后再试");
    this.cancel();
    const utterance = new this.Utterance(text);
    utterance.voice = voice; utterance.lang = voice.lang; utterance.rate = 1;
    this.current = utterance;
    utterance.onend = utterance.onerror = () => { if (this.current === utterance) this.current = null; };
    this.synthesis.speak(utterance);
    return voice.name;
  }
  cancel() {
    // 没有本组件持有的朗读时，不干扰原生或其他插件的语音队列。
    if (this.current) { this.current = null; this.synthesis?.cancel(); }
  }
}

// 捕获只由用户点击启动；释放音轨、处理器、音频上下文与计时器，关闭面板不会继续录音。
class BrowserRecorder {
  constructor(transcriberFactory, onError, onStopped = () => {}, runtime = globalThis) {
    this.transcriberFactory = transcriberFactory; this.onError = onError;
    this.onStopped = onStopped; this.runtime = runtime; this.closed = false;
  }
  async start() {
    try {
      if (this.closed) return;
      this.stream = await this.runtime.navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true }, video: false });
      if (this.closed) { this.stream.getTracks().forEach(track => track.stop()); return; }
      this.context = new this.runtime.AudioContext({ sampleRate: 24000 });
      this.transcriber = this.transcriberFactory(this.context.sampleRate);
      this.source = this.context.createMediaStreamSource(this.stream);
      this.gain = this.context.createGain(); this.gain.gain.value = 0;
      // 固定块处理兼容该桌面构建；每秒约 12 个回调，不创建永久轮询或历史缓存。
      this.processor = this.context.createScriptProcessor(2048, 1, 1);
      this.processor.onaudioprocess = event => {
        if (this.closed) return;
        try { this.transcriber.push(event.inputBuffer.getChannelData(0)); }
        catch (error) { this.onError(error); void this.stop(true).catch(this.onError); }
      };
      this.source.connect(this.processor); this.processor.connect(this.gain); this.gain.connect(this.context.destination);
      await this.context.resume();
      if (this.closed) return;
      this.timer = this.runtime.setTimeout(() => { void this.stop().catch(this.onError); }, 10 * 60 * 1000);
    } catch (error) { await this.stop(true); throw error; }
  }
  async stop(cancel = false) {
    // 即使正在排空尾段，取消也必须中止其上传，而不是只返回旧 Promise。
    if (cancel) this.transcriber?.cancel();
    if (this.stopping) return this.stopping;
    this.closed = true; this.runtime.clearTimeout(this.timer);
    if (this.processor) this.processor.onaudioprocess = null;
    this.source?.disconnect(); this.processor?.disconnect(); this.gain?.disconnect();
    this.stream?.getTracks().forEach(track => track.stop());
    this.stopping = Promise.all([
      this.context?.close().catch(() => {}),
      cancel ? null : this.transcriber?.finish(),
    ]).finally(() => this.onStopped(this));
    return this.stopping;
  }
}

function installAudioPanel({ client, destination, liveEnabled = false, createSpeaker, document: doc = globalThis.document }) {
  const root = doc.createElement("div");
  root.id = "concord-audio-panel";
  const shadow = root.attachShadow({ mode: "open" });
  shadow.innerHTML = `<style>
    :host{position:fixed;right:18px;bottom:68px;z-index:2147483000;font:13px system-ui;color:#e5e7eb}
    button{background:#253247;color:inherit;border:1px solid #536278;border-radius:7px;padding:7px 10px;cursor:pointer}
    section{width:340px;background:#172132;border:1px solid #536278;border-radius:12px;padding:14px;box-shadow:0 10px 35px #0005}
    p{line-height:1.5;margin:9px 0;overflow-wrap:anywhere}textarea{box-sizing:border-box;width:100%;min-height:105px;background:#0e1726;color:inherit;border:1px solid #536278;border-radius:5px;padding:7px}
    .row{display:flex;gap:6px;flex-wrap:wrap;margin:8px 0}[hidden]{display:none!important}small{color:#b6c2d3}
  </style><button id="toggle">音频</button><section hidden>
    <b>ConcordRoute 音频</b><p id="destination"></p>
    <small id="scope"></small>
    <div class="row"><button id="start">开始分段听写</button><button id="stop" disabled>停止并取尾段</button><button id="cancel">取消录音</button></div>
    <textarea aria-label="听写文本与朗读内容" placeholder="听写文字会出现在这里；也可输入要朗读的文字"></textarea>
    <div class="row"><button id="insert">填入输入框</button><button id="speak">本机朗读</button><button id="selected">朗读选中文本</button><button id="quiet">停止朗读</button></div>
    <p id="status" role="status">已加载。录音仅在点击后启动。</p>
  </section>`;
  const get = id => shadow.getElementById(id), text = shadow.querySelector("textarea"), section = shadow.querySelector("section");
  get("destination").textContent = `音频目的地：${destination}`;
  get("scope").textContent = `原生麦克风使用网关。Live ${liveEnabled ? "已接线，仍受上游资格限制" : "尚未启用"}。下方听写每 4 秒更新一段，不是逐词流式协议。`;
  const listeners = [], speaker = createSpeaker ? createSpeaker(error=>status(error.message)) : new LocalSpeaker();
  let recorder, disposed = false;
  function status(message) { if (!disposed) get("status").textContent = message; }
  function on(node, type, handler) { node.addEventListener(type, handler); listeners.push(() => node.removeEventListener(type, handler)); }
  async function upload(wav, signal) {
    const boundary = `audio_${crypto.randomUUID()}`;
    const bytes = await new Blob([
      `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="segment.wav"\r\nContent-Type: audio/wav\r\n\r\n`,
      wav, `\r\n--${boundary}--\r\n`,
    ]).arrayBuffer();
    const response = await client.fetch("/transcribe", { method: "POST", signal,
      headers: { "Content-Type": `multipart/form-data; boundary=${boundary}` }, body: new Uint8Array(bytes) });
    if (!response.ok) throw new Error(`听写服务返回 ${response.status}`);
    const result = await response.json();
    if (typeof result.text !== "string") throw new Error("听写响应缺少文本");
    return result.text;
  }
  async function stop(cancel = false) {
    const current = recorder;
    try { await current?.stop(cancel); status(cancel ? "已取消；未发送的音频已释放" : "已停止，尾段处理完成"); }
    catch (error) { status(error.message); }
    if (recorder === current) recorder = null;
    if (!disposed && !recorder) { get("start").disabled = false; get("stop").disabled = true; }
  }
  on(get("toggle"), "click", () => { section.hidden = !section.hidden; if (section.hidden) { void stop(true); speaker.cancel(); } });
  on(get("start"), "click", async () => {
    if (recorder || disposed) return;
    get("start").disabled = true; get("stop").disabled = false;
    const current = new BrowserRecorder(sampleRate => new SegmentTranscriber({
      sampleRate, upload, onText: value => {
        if (disposed) return;
        if (text.value.length + value.length > 20000) throw new Error("文字已达上限，请保存后开始新段");
        text.value += (text.value ? "\n" : "") + value;
      },
      onError: error => { status(error.message); void stop(true); },
    }), error => status(error.message), stopped => {
      if (recorder === stopped) recorder = null;
      if (!disposed && !recorder) { get("start").disabled = false; get("stop").disabled = true; }
    });
    recorder = current;
    try { await current.start(); if (recorder === current) status("正在录音，每 4 秒提交一段；文字不会自动发出"); }
    catch (error) { await stop(true); status(error.message); }
  });
  on(get("stop"), "click", () => { void stop(false); });
  on(get("cancel"), "click", () => { void stop(true); });
  on(get("quiet"), "click", () => speaker.cancel());
  for (const [id, content] of [["speak", () => text.value], ["selected", () => doc.getSelection()?.toString() || ""]]) {
    on(get(id), "click", () => { try { status(`本机朗读：${speaker.speak(content())}`); } catch (error) { status(error.message); } });
  }
  on(get("insert"), "click", () => {
    const target = doc.querySelector("[data-codex-composer-root] textarea, [data-codex-composer-root] [contenteditable='true'], textarea[data-testid='prompt-textarea'], [contenteditable='true'][role='textbox']");
    if (!target || !text.value) { status("请先打开任务输入框；也可手动复制本框文字"); return; }
    target.focus();
    if (!doc.execCommand("insertText", false, text.value)) status("自动插入未生效，请手动复制；没有发送对话");
    else status("已填入草稿，尚未发送");
  });
  doc.body.appendChild(root);
  return () => { disposed = true; for (const remove of listeners) remove(); speaker.cancel(); void stop(true); root.remove(); };
}

module.exports = { pcm16Wav, SegmentTranscriber, LocalSpeaker, BrowserRecorder, installAudioPanel };
