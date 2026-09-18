"use strict";

const http = require("node:http");
const { randomBytes, timingSafeEqual } = require("node:crypto");
const { startDictationHelper } = require("./dictation-helper.cjs");
const { startLiveHelper } = require("./live-helper.cjs");
const { readUpload } = require("./dictation-helper.cjs");
const { synthesizeSpeech } = require("./windows-speech.cjs");

// 独立用户宿主：长期引导能力仅访问本机，短期上传能力按需续租，无后台上游请求。
async function startAudioHost({ gatewayOrigin, apiKey, controlPort = 0, sidebandPort = 0,
  bootstrap = randomBytes(32).toString("base64url"), revision = null, liveEnabled = false, ...injections }) {
  if (!/^[A-Za-z0-9_-]{43}$/.test(bootstrap)) throw new Error("Invalid audio bootstrap");
  let dictation, live, server, closing, stopped = false, host;
  const sockets = new Set();
  const speech = new Set();
  // 只保存适配器状态与本机报告时间；没有回执时不把宿主存活冒充桌面接线完成。
  let adapter = { state: "unconfirmed", reportedAt: null };
  try {
    dictation = await startDictationHelper({ gatewayOrigin, apiKey, fetchImpl: injections.fetchImpl });
    live = await startLiveHelper({ gatewayOrigin, apiKey, port: sidebandPort,
      fetchImpl: injections.fetchImpl, requestImpl: injections.requestImpl });
    server = http.createServer({ maxHeaderSize: 8192 }, async (req, res) => {
      req.on("error",()=>{});
      function reply(status, data) {
        res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", Connection: "close" });
        res.end(JSON.stringify(data));
      }
      if (stopped) return reply(503, { error: "audio_host_stopped" });
      if (req.headers.host !== host || req.headers.origin) return reply(403, { error: "audio_source_rejected" });
      const actual = Buffer.from(req.headers.authorization || ""), expected = Buffer.from(`Bearer ${bootstrap}`);
      if (actual.length !== expected.length || !timingSafeEqual(actual, expected)) return reply(401, { error: "audio_host_auth_required" });
      if(req.method==="POST"&&req.url==="/audio/speech"){
        if(speech.size>=1)return reply(429,{error:"speech_busy"});
        if(!/^application\/json(?:;|$)/i.test(req.headers["content-type"]||"")||req.headers["content-encoding"])return reply(415,{error:"speech_json_required"});
        const controller=new AbortController();speech.add(controller);
        const timer=setTimeout(()=>controller.abort(),25000);
        const cancel=()=>{if(!res.writableEnded)controller.abort();};
        req.once("aborted",cancel);res.once("close",cancel);
        try{
          const bytes=await readUpload(req,8192,controller.signal);
          const value=JSON.parse(new TextDecoder("utf-8",{fatal:true}).decode(bytes));
          if(typeof value.text!=="string"||!value.text.trim()||value.text.length>400)throw new Error("speech_input_rejected");
          const wav=await (injections.speechImpl||synthesizeSpeech)(value.text,controller.signal);
          controller.signal.throwIfAborted();
          res.writeHead(200,{"Content-Type":"audio/wav","Cache-Control":"no-store",Connection:"close"});res.end(wav);
        }catch{
          if(!res.destroyed&&!res.writableEnded)reply(controller.signal.aborted?504:422,{error:"speech_failed"});
        }finally{clearTimeout(timer);speech.delete(controller);req.off("aborted",cancel);res.off("close",cancel);}
        return;
      }
      if (req.headers["transfer-encoding"] || Number(req.headers["content-length"] || 0) !== 0) return reply(413, { error: "audio_control_body_rejected" });
      if (req.method === "POST" && req.url === "/audio/session") {
        dictation.renew(); live.renew();
        return reply(200, { dictation: dictation.connection, live: live.connection });
      }
      if (req.method === "GET" && req.url === "/audio/status") {
        return reply(200, { ready: true, pid: process.pid, revision, adapter, liveEnabled,
          destination: new URL(gatewayOrigin).origin, live: live.diagnostics() });
      }
      if (req.method === "POST" && /^\/audio\/adapter\/(ready|disposed)$/.test(req.url)) {
        adapter = { state: req.url.split("/").pop(), reportedAt: new Date().toISOString() };
        return reply(200, { accepted: true });
      }
      if (req.method === "POST" && req.url === "/audio/stop") {
        reply(200, { stopped: true });
        res.once("finish", () => { void close(); });
        return;
      }
      reply(404, { error: "audio_control_route_not_found" });
    });
    server.on("connection", socket => {
      sockets.add(socket); socket.once("close", () => sockets.delete(socket)); socket.on("error", () => {});
    });
    server.on("upgrade", (_req, socket) => socket.destroy());
    server.headersTimeout = 5000; server.requestTimeout = 5000; server.maxConnections = 8;
    await new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(controlPort, "127.0.0.1", () => {
        host = `127.0.0.1:${server.address().port}`; server.off("error", reject); resolve();
      });
    });
  } catch (error) { await dictation?.close(); await live?.close(); throw error; }
  function close() {
    if (closing) return closing;
    stopped = true;
    for(const controller of speech)controller.abort();
    closing = Promise.all([dictation.close(), live.close(), new Promise(resolve => {
      for (const socket of sockets) socket.destroy();
      server.close(resolve);
    })]);
    return closing;
  }
  return {
    controlUrl: `http://${host}`, bootstrap, sidebandBaseUrl: live.sidebandBaseUrl,
    close, diagnostics: () => live.diagnostics(),
  };
}

module.exports = { startAudioHost };
