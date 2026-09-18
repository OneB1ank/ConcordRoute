"use strict";

const { matchesDictation, rewrite } = require("./dictation-adapter.cjs");
const installed = new WeakMap();
const LIVE_PATH = "/wham/realtime/calls?intent=quicksilver&architecture=avas";
const LIVE_HEADERS = new Set(["content-type", "openai-alpha", "thread-id", "session-id",
  "x-codex-turn-metadata", "x-oai-attestation"]);

function matchesLive(url, options) {
  if (String(options?.method || "GET").toUpperCase() !== "POST") return false;
  return url === LIVE_PATH || url === `https://chatgpt.com/backend-api${LIVE_PATH}`;
}

function rewriteLive(options, connection) {
  const endpoint = new URL(connection.helperUrl);
  if (endpoint.protocol !== "http:" || endpoint.hostname !== "127.0.0.1" || !endpoint.port
      || endpoint.pathname !== "/audio/live" || endpoint.search || endpoint.hash || endpoint.username || endpoint.password) {
    throw new Error("Invalid dedicated Live endpoint");
  }
  if (!/^[A-Za-z0-9_-]{24,128}$/.test(connection.localCapability || "")
      || !Number.isFinite(connection.expiresAt) || Date.now() >= connection.expiresAt) {
    throw new Error("Live capability expired");
  }
  if (typeof options.body !== "string" || new TextEncoder().encode(options.body).length > 1024 * 1024) {
    throw new Error("Invalid Live request body");
  }
  const parsed = JSON.parse(options.body);
  if (!parsed || typeof parsed.sdp !== "string" || !parsed.sdp.trim()
      || !parsed.session || typeof parsed.session !== "object" || Array.isArray(parsed.session)) {
    throw new Error("Invalid Live session");
  }
  const headers = {};
  const source = options.headers instanceof Headers ? [...options.headers]
    : Array.isArray(options.headers) ? options.headers : Object.entries(options.headers || {});
  for (const [rawName, value] of source) {
    const name = rawName.toLowerCase();
    if (!LIVE_HEADERS.has(name)) continue;
    if (Object.hasOwn(headers, name) || typeof value !== "string"
        || /[\r\n\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(value) || value.length > 16384) {
      throw new Error("Invalid Live header");
    }
    headers[name] = value;
  }
  if (!/^application\/json(?:;|$)/i.test(headers["content-type"] || "")) throw new Error("Live JSON required");
  headers.Authorization = `Bearer ${connection.localCapability}`;
  return { url: endpoint.href, options: { ...options, headers, credentials: "omit", redirect: "error" } };
}

// @project-doc docs/interfaces/audio_transcription.md#desktop_audio_adapter
// 配置按需由可信宿主交付；非音频调用保持同步返回与原参数，不探测、不刷新账号。
function installAudioAdapter(client, getConnection, { liveEnabled = true } = {}) {
  if (!client || typeof client.fetch !== "function" || typeof getConnection !== "function" || installed.has(client)) {
    throw new Error("Invalid or already installed audio adapter");
  }
  const previous = Object.getOwnPropertyDescriptor(client, "fetch");
  if (previous && (!("value" in previous) || !previous.configurable)) throw new Error("HTTP client is not restorable");
  const original = client.fetch, active = new Set();
  let enabled = true;
  function wrapped(url, options = {}) {
    const live = liveEnabled && matchesLive(url, options), dictation = matchesDictation(url, options);
    if (!enabled || (!live && !dictation)) return Reflect.apply(original, this, [url, options]);
    const controller = new AbortController();
    const signal = options.signal ? AbortSignal.any([options.signal, controller.signal]) : controller.signal;
    active.add(controller);
    return Promise.resolve().then(async () => {
      signal.throwIfAborted();
      const connection = await getConnection(signal);
      signal.throwIfAborted();
      const mapped = live ? rewriteLive({ ...options, signal }, connection.live)
        : rewrite(url, { ...options, signal }, { ...connection.dictation, enabled: true });
      return Reflect.apply(original, this, [mapped.url, mapped.options]);
    }).finally(() => active.delete(controller));
  }
  Object.defineProperty(client, "fetch", { value: wrapped, configurable: true, writable: true });
  installed.set(client, wrapped);
  return function uninstall() {
    enabled = false;
    for (const controller of active) controller.abort();
    if (client.fetch !== wrapped) return false;
    if (previous) Object.defineProperty(client, "fetch", previous);
    else delete client.fetch;
    installed.delete(client);
    return true;
  };
}

module.exports = { matchesLive, rewriteLive, installAudioAdapter };
