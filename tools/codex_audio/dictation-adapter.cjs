"use strict";

const { validMultipart } = require("./dictation-contract.cjs");
const installations = new WeakMap();

function matchesDictation(url, options) {
  if (String(options?.method || "GET").toUpperCase() !== "POST") return false;
  if (url === "/transcribe") return true;
  try {
    const parsed = new URL(url);
    return parsed.origin === "https://chatgpt.com"
      && parsed.pathname === "/backend-api/transcribe"
      && !parsed.username && !parsed.password && !parsed.search && !parsed.hash;
  } catch { return false; }
}

function rewrite(url, options, config) {
  if (!config.enabled || !matchesDictation(url, options)) return { url, options };
  options.signal?.throwIfAborted();
  const endpoint = new URL(config.helperUrl);
  if (endpoint.protocol !== "http:" || endpoint.hostname !== "127.0.0.1"
      || !endpoint.port || endpoint.pathname !== "/dictation/transcribe"
      || endpoint.username || endpoint.password || endpoint.search || endpoint.hash) {
    throw new Error("Invalid dedicated loopback endpoint");
  }
  if (!/^[A-Za-z0-9_-]{24,128}$/.test(config.localCapability || "")) {
    throw new Error("Local capability is required");
  }
  if (config.expiresAt != null && (!Number.isFinite(config.expiresAt) || Date.now() >= config.expiresAt)) {
    throw new Error("Local audio capability expired");
  }
  const body = options.body;
  if (!(body instanceof ArrayBuffer || ArrayBuffer.isView(body))
      || !body.byteLength || body.byteLength > 26 * 1024 * 1024) {
    throw new Error("Invalid or oversized binary upload");
  }
  const entries = options.headers instanceof Headers ? [...options.headers]
    : Array.isArray(options.headers) ? options.headers : Object.entries(options.headers || {});
  const types = entries.filter(([name]) => name.toLowerCase() === "content-type");
  if (types.length !== 1 || !validMultipart(types[0][1])) throw new Error("Invalid multipart content type");
  return {
    url: endpoint.href,
    options: {
      ...options,
      credentials: "omit",
      redirect: "error",
      // 独立 provider 不携带官方登录、Cookie、证明及内部认证指令。
      headers: {
        "Content-Type": types[0][1],
        Accept: "application/json",
        Authorization: `Bearer ${config.localCapability}`,
      },
    },
  };
}

function wrapFetch(original, config) {
  return function wrappedFetch(url, options = {}) {
    if (!config.enabled || !matchesDictation(url, options)) return Reflect.apply(original, this, [url, options]);
    try {
      const mapped = rewrite(url, options, config);
      return Reflect.apply(original, this, [mapped.url, mapped.options]);
    } catch (error) { return Promise.reject(error); }
  };
}

// 宿主须先按实际桌面构建识别 HTTP 单例；本模块不扫描、注入或改写安装包。
// 显式安装才启用，保持非听写请求的调用方式、参数、返回对象和接收者原样不变。
function installDictationAdapter(client, connection) {
  if (!client || typeof client.fetch !== "function" || installations.has(client)) {
    throw new Error("Invalid or already adapted HTTP client");
  }
  const previous = Object.getOwnPropertyDescriptor(client, "fetch");
  if (previous && (!("value" in previous) || !previous.configurable)) {
    throw new Error("HTTP fetch cannot be safely restored");
  }
  const original = client.fetch;
  const active = new Set();
  const config = Object.freeze({ ...connection, enabled: true });
  let enabled = true;
  const wrapped = function (url, options = {}) {
    if (!enabled || !matchesDictation(url, options)) return Reflect.apply(original, this, [url, options]);
    const controller = new AbortController();
    const signal = options.signal ? AbortSignal.any([options.signal, controller.signal]) : controller.signal;
    let mapped;
    try { mapped = rewrite(url, { ...options, signal }, config); } catch (error) { return Promise.reject(error); }
    active.add(controller);
    return Promise.resolve().then(() => {
      signal.throwIfAborted();
      return Reflect.apply(original, this, [mapped.url, mapped.options]);
    })
      .finally(() => active.delete(controller));
  };
  Object.defineProperty(client, "fetch", { value: wrapped, writable: true, configurable: true });
  installations.set(client, wrapped);
  return function uninstall() {
    enabled = false;
    for (const controller of active) controller.abort();
    if (client.fetch !== wrapped) return false; // 不覆盖后来安装的其它包装器。
    if (previous) Object.defineProperty(client, "fetch", previous);
    else delete client.fetch;
    installations.delete(client);
    return true;
  };
}

module.exports = { matchesDictation, rewrite, wrapFetch, installDictationAdapter };
