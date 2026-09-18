"use strict";

const fs = require("node:fs");
const path = require("node:path");

// 固定已核验构建，升级后停用而非猜测混淆导出名；不扫描聊天或注入凭据到网页。
const CONTRACT = {
  asset: "app-initial-92cbfeba4f7c.js",
  sha256: "08fd31b7454a15fee5447bb4d073a5227165df0819eabaeaef8694ed767f5f5b",
  clientExport: "gJt",
};
function buildUserScript({ controlUrl, bootstrap, destination, liveEnabled = false }) {
  const url = new URL(controlUrl);
  if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || !url.port
      || url.pathname !== "/" || url.search || url.hash || url.username || url.password
      || !/^[A-Za-z0-9_-]{43}$/.test(bootstrap)) throw new Error("Invalid audio bootstrap");
  const modules = ["dictation-contract.cjs", "dictation-adapter.cjs", "desktop-audio-adapter.cjs", "audio-ui.cjs", "windows-speaker.cjs"];
  const factories = modules.map(name => `${JSON.stringify("./" + name)}: function(module,exports,require){\n${fs.readFileSync(path.join(__dirname, name), "utf8")}\n}`).join(",\n");
  return `// ConcordRoute Audio 0.2.0 — 本地独立音频，不修改统计脚本。
// 此文件含本机引导能力，仅当前用户可读；不含网关 Key，请勿分享本文件。
void (async function () {
  if (window.top !== window || location.protocol !== "app:" || location.hostname !== "-") return;
  const slot = "__concordAudio";
  const previous = window[slot];
  const previousDisposal = previous?.dispose();
  const state = { status: "loading", disposed: false, dispose() {
    if (this.disposed) return;
    this.disposed = true; this.unmount?.(); this.uninstall?.();
    if (this.onPageHide) window.removeEventListener("pagehide",this.onPageHide);
    return this.report?.("disposed");
  } };
  window[slot] = state;
  const factories = {${factories}}, cache = {};
  function require(name) {
    if (!cache[name]) { const module = {exports:{}}; cache[name] = module; factories[name](module,module.exports,require); }
    return cache[name].exports;
  }
  try {
    await previousDisposal;
    const asset = ${JSON.stringify(CONTRACT.asset)};
    const resources = [...document.querySelectorAll("script[src]")].map(x => x.src)
      .concat(performance.getEntriesByType("resource").map(x => x.name));
    if (!resources.some(x => new URL(x,location.href).pathname === "/assets/" + asset))
      throw new Error("桌面构建尚未核验，音频适配器保持停用");
    const native = await import("/assets/" + asset);
    const client = native[${JSON.stringify(CONTRACT.clientExport)}]?.getInstance?.();
    if (!client || typeof client.fetch !== "function") throw new Error("桌面 HTTP 服务尚未就绪");
    if (state.disposed) return;
    const nativeFetch = client.fetch.bind(client);
    state.report = async status => {
      try {
        const response = await nativeFetch(${JSON.stringify(controlUrl + "/audio/adapter/")}+status, {
          method:"POST",signal:AbortSignal.timeout(3000),
          headers:{Authorization:${JSON.stringify("Bearer " + bootstrap)}},credentials:"omit",redirect:"error"
        });
        await response.arrayBuffer();
      } catch {}
    };
    async function connection(signal) {
      const result = await nativeFetch(${JSON.stringify(controlUrl + "/audio/session")}, {
        method:"POST",signal,headers:{Authorization:${JSON.stringify("Bearer " + bootstrap)}},
        credentials:"omit",redirect:"error"
      });
      if (!result.ok) throw new Error("本机音频宿主未就绪："+result.status);
      // 宿主重启后下次实际音频调用重新报告，不增加后台轮询。
      void state.report("ready");
      return result.json();
    }
    state.uninstall = require("./desktop-audio-adapter.cjs").installAudioAdapter(client,connection,{liveEnabled:${!!liveEnabled}});
    state.unmount = require("./audio-ui.cjs").installAudioPanel({
      client,destination:${JSON.stringify(destination)},liveEnabled:${!!liveEnabled},
      createSpeaker: onError => new (require("./windows-speaker.cjs").WindowsSpeaker)({
        fetch:nativeFetch,connection:async()=>({url:${JSON.stringify(controlUrl)},capability:${JSON.stringify(bootstrap)}}),onError
      })
    });
    state.status = "ready";
    void state.report("ready");
    state.onPageHide = () => state.dispose();
    window.addEventListener("pagehide", state.onPageHide, {once:true});
  } catch(error) {
    state.dispose(); state.status="failed"; state.error=String(error.message);
    console.warn("[ConcordRoute Audio] "+state.error);
  }
})();\n`;
}
module.exports = { buildUserScript, CONTRACT };
