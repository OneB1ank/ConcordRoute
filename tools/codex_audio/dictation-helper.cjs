"use strict";

const http = require("node:http");
const { randomBytes, timingSafeEqual } = require("node:crypto");
const { validMultipart } = require("./dictation-contract.cjs");

const UPLOAD_LIMIT = 26 * 1024 * 1024;
const RESPONSE_LIMIT = 1024 * 1024;
const CAPABILITY_TTL = 30 * 60 * 1000;
const UPLOAD_PATH = "/dictation/transcribe";
const FAULT = Symbol("local-audio-error");

function fault(status, code) {
  return Object.assign(new Error(code), { status, code, [FAULT]: true });
}

function readUpload(req, limit, signal) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let bytes = 0;
    function finish(err) {
      req.off("data", onData);
      req.off("end", onEnd);
      req.off("error", onError);
      signal.removeEventListener("abort", onAbort);
      if (err) {
        req.pause();
        reject(err);
      } else resolve(Buffer.concat(chunks, bytes));
    }
    function onData(chunk) {
      bytes += chunk.length;
      if (bytes > limit) finish(fault(413, "audio_upload_too_large"));
      else chunks.push(chunk);
    }
    function onEnd() { finish(); }
    function onError() { finish(fault(400, "audio_upload_interrupted")); }
    function onAbort() { finish(signal.reason); }
    req.on("data", onData).once("end", onEnd).once("error", onError);
    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted) onAbort();
  });
}

async function readResponse(response, limit, signal) {
  const reader = response.body?.getReader();
  if (!reader) throw fault(502, "audio_gateway_invalid_response");
  const chunks = [];
  let bytes = 0;
  const onAbort = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener("abort", onAbort, { once: true });
  try {
    for (;;) {
      signal.throwIfAborted();
      const part = await reader.read();
      signal.throwIfAborted();
      if (part.done) return Buffer.concat(chunks, bytes);
      bytes += part.value.byteLength;
      if (bytes > limit) throw fault(502, "audio_gateway_response_too_large");
      chunks.push(Buffer.from(part.value));
    }
  } finally {
    signal.removeEventListener("abort", onAbort);
    await reader.cancel().catch(() => {});
    reader.releaseLock();
  }
}

// @project-doc docs/interfaces/audio_transcription.md#desktop_audio_adapter
// 本模块只接收用户已录制的数据，不访问麦克风、不读取官方登录状态、不打印凭据。
async function startDictationHelper({
  gatewayOrigin, apiKey, allowedOrigins = [], port = 0,
  timeoutMs = 120000, capabilityTtlMs = CAPABILITY_TTL,
  uploadLimit = UPLOAD_LIMIT, responseLimit = RESPONSE_LIMIT,
  fetchImpl = globalThis.fetch,
}) {
  const target = new URL(gatewayOrigin);
  if (target.protocol !== "https:" || target.username || target.password
      || target.pathname !== "/" || target.search || target.hash) {
    throw new Error("gatewayOrigin must be an HTTPS origin without a path");
  }
  if (typeof apiKey !== "string" || !/^[\x21-\x7e]{8,2048}$/.test(apiKey)) {
    throw new Error("Invalid gateway credential");
  }
  for (const [value, max] of [
    [timeoutMs, 120000], [capabilityTtlMs, CAPABILITY_TTL],
    [uploadLimit, UPLOAD_LIMIT], [responseLimit, RESPONSE_LIMIT],
  ]) {
    if (!Number.isSafeInteger(value) || value < 1 || value > max) throw new Error("Invalid audio limit");
  }
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error("Invalid loopback port");
  if (!Array.isArray(allowedOrigins) || allowedOrigins.some(origin => {
    if (typeof origin !== "string" || /[\x00-\x20\x7f]/.test(origin)) return true;
    try { return new URL(origin).origin !== origin || origin === "null"; } catch { return true; }
  })) throw new Error("Explicit non-opaque origins are required");

  const allowed = new Set(allowedOrigins);
  const capability = randomBytes(32).toString("base64url");
  const expectedAuth = Buffer.from(`Bearer ${capability}`);
  let expiresAt = Date.now() + capabilityTtlMs;
  const active = new Set();
  let host;
  let closed = false;
  let closing;
  const upstream = new URL("/transcribe", target).href;

  function reply(res, status, body) {
    if (res.destroyed || res.writableEnded) return;
    res.writeHead(status, {
      "Content-Type": "application/json",
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
      Connection: "close",
    });
    res.end(JSON.stringify(body));
  }

  const server = http.createServer({ maxHeaderSize: 8192 }, async (req, res) => {
    // 先验证来源及独立能力令牌，再读音频；无 Origin 的桌面主进程也必须认证。
    // aborted 后仍可能发出 error，避免清理读监听器后出现未处理事件。
    req.on("error", () => { if (!res.writableEnded) res.destroy(); });
    let controller;
    let timer;
    let onDisconnect;
    try {
      if (closed || Date.now() >= expiresAt) throw fault(401, "audio_capability_expired");
      if (req.headers.host !== host) throw fault(403, "audio_host_rejected");
      if (req.url !== UPLOAD_PATH) throw fault(404, "audio_route_not_found");
      const origin = req.headers.origin;
      if (origin && !allowed.has(origin)) throw fault(403, "audio_origin_rejected");
      if (origin) {
        res.setHeader("Access-Control-Allow-Origin", origin);
        res.setHeader("Vary", "Origin");
      }
      if (req.method === "OPTIONS") {
        const names = String(req.headers["access-control-request-headers"] || "")
          .toLowerCase().split(",").map(x => x.trim()).filter(Boolean);
        if (!origin || req.headers["access-control-request-method"] !== "POST"
            || names.some(x => !["authorization", "content-type", "accept"].includes(x))) {
          throw fault(403, "audio_preflight_rejected");
        }
        res.writeHead(204, {
          "Access-Control-Allow-Methods": "POST",
          "Access-Control-Allow-Headers": "Authorization, Content-Type, Accept",
          "Cache-Control": "no-store",
        });
        res.end();
        return;
      }
      if (req.method !== "POST") throw fault(405, "audio_method_rejected");
      const auth = Buffer.from(req.headers.authorization || "");
      if (auth.length !== expectedAuth.length || !timingSafeEqual(auth, expectedAuth)) {
        throw fault(401, "audio_capability_required");
      }
      if (req.headers["content-encoding"] || !validMultipart(req.headers["content-type"])) {
        throw fault(415, "audio_multipart_required");
      }
      if (Number(req.headers["content-length"] || 0) > uploadLimit) {
        throw fault(413, "audio_upload_too_large");
      }
      if (active.size >= 2) throw fault(429, "audio_helper_busy");
      controller = new AbortController();
      active.add(controller);
      onDisconnect = () => {
        if (!res.writableEnded) controller.abort(fault(499, "audio_client_disconnected"));
      };
      req.once("aborted", onDisconnect);
      res.once("close", onDisconnect);
      timer = setTimeout(() => controller.abort(fault(504, "audio_timeout")), timeoutMs);
      timer.unref();
      const body = await readUpload(req, uploadLimit, controller.signal);
      if (!body.length) throw fault(400, "audio_upload_empty");
      controller.signal.throwIfAborted();
      const response = await fetchImpl(upstream, {
        method: "POST", body, signal: controller.signal, redirect: "manual",
        headers: {
          Authorization: `Bearer ${apiKey}`,
          "Content-Type": req.headers["content-type"],
          Accept: "application/json",
        },
      });
      if (!response.ok || !/^application\/json(?:;|$)/i.test(response.headers.get("content-type") || "")) {
        await response.body?.cancel();
        throw fault(response.status >= 400 && response.status <= 599 ? response.status : 502,
          "audio_gateway_rejected");
      }
      const bytes = await readResponse(response, responseLimit, controller.signal);
      let result;
      try { result = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)); } catch {
        throw fault(502, "audio_gateway_invalid_json");
      }
      if (typeof result?.text !== "string") throw fault(502, "audio_gateway_missing_text");
      reply(res, 200, { text: result.text });
    } catch (error) {
      const failure = controller?.signal.aborted ? controller.signal.reason : error;
      // 不把上游异常对象、转录文本或令牌放进诊断响应。
      reply(res, failure?.[FAULT] ? failure.status : 502,
        { error: { code: failure?.[FAULT] ? failure.code : "audio_gateway_unavailable" } });
    } finally {
      clearTimeout(timer);
      if (onDisconnect) {
        req.off("aborted", onDisconnect);
        res.off("close", onDisconnect);
      }
      if (controller) active.delete(controller);
    }
  });
  server.headersTimeout = 10000;
  server.requestTimeout = 120000;
  server.maxConnections = 8;
  // 不提供 WS 或任意目标代理，Live 的信令与 Sideband 必须另行集成。
  server.on("upgrade", (_req, socket) => socket.destroy());
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", () => {
      host = `127.0.0.1:${server.address().port}`;
      server.off("error", reject);
      resolve();
    });
  });
  function close() {
    if (closing) return closing;
    closed = true;
    apiKey = "";
    for (const controller of active) controller.abort(fault(503, "audio_helper_stopped"));
    closing = new Promise(resolve => {
      server.close(resolve);
      server.closeAllConnections();
    });
    return closing;
  }
  return {
    // 只向可信宿主交付短期能力；独立宿主以自己的本地认证再交付给适配器。
    get connection() { return Object.freeze({ helperUrl: `http://${host}${UPLOAD_PATH}`, localCapability: capability, expiresAt }); },
    renew() { if (closed) throw new Error("Audio helper stopped"); expiresAt = Date.now() + capabilityTtlMs; },
    close,
  };
}

// Live 信令复用有界读取；错误标记保持模块私有，外部不回显异常原文。
module.exports = { startDictationHelper, readUpload, readResponse, isAudioFault: error => !!error?.[FAULT] };
