"use strict";

const http = require("node:http");
const https = require("node:https");
const { randomBytes, timingSafeEqual, createHash } = require("node:crypto");
const { readUpload, readResponse, isAudioFault } = require("./dictation-helper.cjs");
const FORWARD_HEADERS = ["openai-alpha", "thread-id", "session-id", "x-codex-turn-metadata", "x-oai-attestation"];

function sameSecret(actual, expected) {
  if (typeof actual !== "string") return false;
  const a = Buffer.from(actual), b = Buffer.from(expected);
  return a.length === b.length && timingSafeEqual(a, b);
}
function fail(status, code) { return Object.assign(new Error(code), { audioStatus: status, audioCode: code }); }

// @project-doc docs/interfaces/audio_transcription.md#desktop_audio_adapter
// 创建持有网关 Key；Sideband 仅接受该 Key 与该进程创建的 callId，不构成通用 WS 代理。
async function startLiveHelper({
  gatewayOrigin, apiKey, port = 0, capabilityTtlMs = 30 * 60 * 1000,
  timeoutMs = 30000, callTtlMs = 30 * 60 * 1000,
  fetchImpl = globalThis.fetch, requestImpl = https.request,
}) {
  const gateway = new URL(gatewayOrigin);
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error("Invalid Live port");
  if (gateway.protocol !== "https:" || gateway.pathname !== "/" || gateway.search || gateway.hash
      || gateway.username || gateway.password || !/^[\x21-\x7e]{8,2048}$/.test(apiKey || "")) throw new Error("Invalid Live provider");
  for (const value of [capabilityTtlMs, timeoutMs, callTtlMs]) {
    if (!Number.isSafeInteger(value) || value < 1 || value > 30 * 60 * 1000) throw new Error("Invalid Live limit");
  }
  const capability = randomBytes(32).toString("base64url");
  const calls = new Map(), pending = new Set(), sockets = new Set();
  let expiresAt = Date.now() + capabilityTtlMs, host, closing, stopped = false;
  let created = 0, attached = 0, failures = 0;
  function reply(res, status, code) {
    if (!res.destroyed && !res.writableEnded) {
      res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", Connection: "close" });
      res.end(JSON.stringify({ error: { code } }));
    }
  }
  const server = http.createServer({ maxHeaderSize: 24576 }, async (req, res) => {
    req.on("error", () => res.destroy());
    const controller = new AbortController();
    let timer, admitted = false;
    const disconnect = () => { if (!res.writableEnded) controller.abort(); };
    try {
      if (stopped || Date.now() >= expiresAt) throw fail(401, "audio_capability_expired");
      if (req.headers.host !== host || req.headers.origin) throw fail(403, "audio_source_rejected");
      if (req.url !== "/audio/live" || req.method !== "POST") throw fail(404, "audio_route_not_found");
      if (!sameSecret(req.headers.authorization, `Bearer ${capability}`)) throw fail(401, "audio_capability_required");
      if (req.headers["content-encoding"] || !/^application\/json(?:;|$)/i.test(req.headers["content-type"] || "")) {
        throw fail(415, "live_json_required");
      }
      if (Number(req.headers["content-length"] || 0) > 1024 * 1024) throw fail(413, "live_request_too_large");
      if (pending.size >= 2 || calls.size + pending.size >= 4) throw fail(429, "live_helper_busy");
      pending.add(controller); admitted = true;
      timer = setTimeout(() => controller.abort(), timeoutMs); timer.unref();
      req.once("aborted", disconnect); res.once("close", disconnect);
      const body = await readUpload(req, 1024 * 1024, controller.signal);
      let request;
      try { request = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(body)); } catch { throw fail(400, "invalid_live_json"); }
      if (!request || typeof request.sdp !== "string" || !request.sdp.trim()
          || !request.session || typeof request.session !== "object" || Array.isArray(request.session)) throw fail(400, "invalid_live_session");
      const identityHeaders = {};
      for (const name of FORWARD_HEADERS) if (req.headers[name]) identityHeaders[name] = req.headers[name];
      const response = await fetchImpl(new URL("/v1/live", gateway), {
        method: "POST", body, signal: controller.signal, redirect: "manual",
        headers: { ...identityHeaders, Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
      });
      if (response.status !== 201 || !/^application\/sdp(?:;|$)/i.test(response.headers.get("content-type") || "")) {
        await response.body?.cancel();
        throw fail(response.status >= 400 && response.status <= 599 ? response.status : 502, "live_gateway_rejected");
      }
      const location = new URL(response.headers.get("location") || "", gateway);
      const match = /^\/v1\/live\/(rtc_[A-Za-z0-9_-]{1,160})$/.exec(location.pathname);
      if (location.origin !== gateway.origin || !match || location.search || location.hash || location.username || location.password) {
        await response.body?.cancel(); throw fail(502, "live_invalid_location");
      }
      const answer = await readResponse(response, 1024 * 1024, controller.signal);
      if (!answer.toString("utf8").startsWith("v=0")) throw fail(502, "live_invalid_sdp");
      controller.signal.throwIfAborted();
      if (calls.has(match[1])) throw fail(502, "live_duplicate_call");
      const record = { identityHeaders, connecting: false, upstream: null, client: null, expire: null };
      calls.set(match[1], record);
      record.expire = setTimeout(() => release(match[1]), callTtlMs); record.expire.unref();
      created++;
      res.writeHead(201, { "Content-Type": "application/sdp", Location: `/v1/live/${match[1]}`, "Cache-Control": "no-store" });
      res.end(answer);
    } catch (error) {
      failures++;
      reply(res, controller.signal.aborted ? 504 : error.audioStatus || (isAudioFault(error) ? error.status : 502),
        controller.signal.aborted ? "live_cancelled_or_timed_out" : error.audioCode || "live_gateway_unavailable");
    } finally {
      clearTimeout(timer);
      req.off("aborted", disconnect); res.off("close", disconnect);
      if (admitted) pending.delete(controller);
    }
  });
  function release(id, preserveClient = false) {
    const record = calls.get(id);
    if (!record) return;
    calls.delete(id); clearTimeout(record.expire); clearTimeout(record.opening);
    record.request?.destroy(); record.upstream?.destroy();
    if (!preserveClient) record.client?.destroy();
  }
  server.on("connection", socket => {
    sockets.add(socket); socket.once("close", () => sockets.delete(socket));
    socket.on("error", () => {});
  });
  server.on("upgrade", (req, client, head) => {
    function reject(status) { client.end(`HTTP/1.1 ${status} Rejected\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`); }
    if (stopped || req.headers.host !== host || req.headers.origin) return reject(403);
    if (!sameSecret(req.headers.authorization, `Bearer ${apiKey}`)) return reject(401);
    const match = /^\/v1\/live\/(rtc_[A-Za-z0-9_-]{1,160})$/.exec(req.url);
    const record = match && calls.get(match[1]);
    if (!record) return reject(404);
    if (record.connecting) return reject(409);
    if (req.headers.upgrade?.toLowerCase() !== "websocket" || req.headers["sec-websocket-version"] !== "13"
        || !/^[A-Za-z0-9+/]{22}==$/.test(req.headers["sec-websocket-key"] || "")) return reject(400);
    record.connecting = true; record.client = client;
    const headers = {
      ...record.identityHeaders, Authorization: `Bearer ${apiKey}`, Connection: "Upgrade", Upgrade: "websocket",
      "Sec-WebSocket-Key": req.headers["sec-websocket-key"], "Sec-WebSocket-Version": "13",
    };
    if (req.headers["sec-websocket-protocol"]) headers["Sec-WebSocket-Protocol"] = req.headers["sec-websocket-protocol"];
    let upstream;
    function failed(status) {
      if (!calls.has(match[1])) return;
      failures++;
      // 先将错误响应写完，避免 destroy 吞掉真实状态码；本地连接仍有界关闭。
      reject(status);
      release(match[1], true);
      const deadline = setTimeout(() => client.destroy(), 1000); deadline.unref();
      client.once("close", () => clearTimeout(deadline));
    }
    try { upstream = requestImpl(new URL(req.url, gateway), { method: "GET", headers }); }
    catch { failed(502); return; }
    record.request = upstream;
    const opening = record.opening = setTimeout(() => failed(504), timeoutMs); opening.unref();
    upstream.once("upgrade", (response, socket, upstreamHead) => {
      clearTimeout(opening);
      if (client.destroyed || !calls.has(match[1])) { socket.destroy(); return; }
      const accept = createHash("sha1").update(req.headers["sec-websocket-key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest("base64");
      const selected = response.headers["sec-websocket-protocol"];
      const offered = String(req.headers["sec-websocket-protocol"] || "").split(",").map(x => x.trim());
      if (response.statusCode !== 101 || response.headers.upgrade?.toLowerCase() !== "websocket"
          || response.headers["sec-websocket-accept"] !== accept
          || (selected && !offered.includes(selected))) { socket.destroy(); failed(502); return; }
      record.upstream = socket; attached++;
      const lines = ["HTTP/1.1 101 Switching Protocols", "Upgrade: websocket", "Connection: Upgrade"];
      for (const name of ["sec-websocket-accept", "sec-websocket-protocol"]) {
        const value = response.headers[name];
        if (typeof value === "string" && !/[\r\n]/.test(value)) lines.push(`${name}: ${value}`);
      }
      client.write(lines.join("\r\n") + "\r\n\r\n");
      if (upstreamHead.length) client.write(upstreamHead);
      if (head.length) socket.write(head);
      // 使用流背压传递原始控制帧，不缓存正文、不重写会话或工具事件。
      client.pipe(socket); socket.pipe(client);
      socket.once("close", () => release(match[1]));
      socket.on("error", () => release(match[1]));
    });
    upstream.once("response", response => {
      response.resume();
      failed(response.statusCode >= 400 && response.statusCode < 600 ? response.statusCode : 502);
    });
    upstream.once("error", () => failed(502));
    client.once("close", () => { clearTimeout(opening); release(match[1]); });
    try { upstream.end(); } catch { failed(502); }
  });
  server.headersTimeout = 10000; server.requestTimeout = 45000; server.maxConnections = 12;
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", () => { host = `127.0.0.1:${server.address().port}`; server.off("error", reject); resolve(); });
  });
  return {
    get connection() { return { helperUrl: `http://${host}/audio/live`, localCapability: capability, expiresAt }; },
    sidebandBaseUrl: `ws://${host}/v1`,
    // 仅可信宿主在本地用户操作时续租，不发送任何上游探测。
    renew() { if (stopped) throw new Error("Live helper stopped"); expiresAt = Date.now() + capabilityTtlMs; },
    diagnostics: () => ({ created, attached, failures, calls: calls.size, pending: pending.size }),
    close() {
      if (closing) return closing;
      stopped = true;
      for (const request of pending) request.abort();
      for (const id of calls.keys()) release(id);
      for (const socket of sockets) socket.destroy();
      apiKey = "";
      closing = new Promise(resolve => server.close(resolve));
      return closing;
    },
  };
}

module.exports = { startLiveHelper };
