"use strict";

const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { execFileSync } = require("node:child_process");
const { startAudioHost } = require("./audio-host.cjs");
const { buildUserScript } = require("./build-user-script.cjs");
const { loadBootstrap, runtimeRevision } = require("./runtime-state.cjs");

// ACL 必须先验证再写能力文件；网关 Key 始终只读自当前用户现有 auth.json 并留在进程内。
function privateDirectory(directory) {
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 });
  if (process.platform === "win32") {
    const sid = execFileSync("whoami.exe", ["/user", "/fo", "csv", "/nh"], { encoding: "utf8", windowsHide: true })
      .match(/S-1-5-[\d-]+/)?.[0];
    if (!sid) throw new Error("User ACL unavailable");
    execFileSync("icacls.exe", [directory, "/inheritance:r", "/grant:r", `*${sid}:(OI)(CI)F`, "*S-1-5-18:(OI)(CI)F"], { windowsHide: true, stdio: "ignore" });
  }
}

async function main() {
  const directory = process.argv[2];
  if (!directory || !path.isAbsolute(directory)) throw new Error("Absolute runtime directory required");
  privateDirectory(directory);
  const settings = JSON.parse(fs.readFileSync(path.join(directory, "settings.json"), "utf8").replace(/^\uFEFF/, ""));
  const credential = JSON.parse(fs.readFileSync(path.join(os.homedir(), ".codex", "auth.json"), "utf8")).OPENAI_API_KEY;
  const revision = runtimeRevision(__dirname);
  const host = await startAudioHost({ gatewayOrigin: settings.gatewayOrigin, apiKey: credential,
    controlPort: settings.controlPort, sidebandPort: settings.sidebandPort,
    bootstrap: loadBootstrap(directory), revision, liveEnabled: !!settings.liveEnabled });
  try {
  const statePath = path.join(directory, "runtime.json");
  const source = buildUserScript({ controlUrl: host.controlUrl, bootstrap: host.bootstrap, destination: settings.gatewayOrigin, liveEnabled: settings.liveEnabled });
  // 安装能力跨宿主重启保留，已载入的脚本继续按需获取新的短期上传能力。
  fs.writeFileSync(path.join(directory, "audio-user-script.js"), source, { mode: 0o600 });
  if (settings.userScriptPath) {
    const expected = path.join(os.homedir(), "AppData", "Roaming", "CodexElves", "user_scripts", "concord-audio.js");
    if (path.resolve(settings.userScriptPath) !== expected) { await host.close(); throw new Error("Unexpected script destination"); }
    // 先创建空文件并收紧 ACL，再写短期进程能力，避免继承宽泛读取权限。
    fs.writeFileSync(expected, "", { mode: 0o600 });
    const sid = execFileSync("whoami.exe", ["/user", "/fo", "csv", "/nh"], { encoding: "utf8", windowsHide: true })
      .match(/S-1-5-[\d-]+/)?.[0];
    if (!sid) { await host.close(); throw new Error("User ACL unavailable"); }
    execFileSync("icacls.exe", [expected, "/inheritance:r", "/grant:r", `*${sid}:F`, "*S-1-5-18:F"], { windowsHide: true, stdio: "ignore" });
    fs.writeFileSync(expected, source, { mode: 0o600 });
  }
  fs.writeFileSync(statePath, JSON.stringify({ pid: process.pid, controlUrl: host.controlUrl,
    bootstrap: host.bootstrap, sidebandBaseUrl: host.sidebandBaseUrl, revision }), { mode: 0o600 });
  function removeOwnState() {
    try {
      if (JSON.parse(fs.readFileSync(statePath, "utf8")).pid === process.pid) fs.unlinkSync(statePath);
    } catch {}
  }
  async function close() {
    await host.close();
    removeOwnState();
  }
  process.once("SIGINT", close); process.once("SIGTERM", close);
  process.once("beforeExit", removeOwnState);
  } catch (error) {
    await host.close();
    throw error;
  }
}

if (require.main === module) main().catch(() => {
  // 只提供稳定错误码，不把 auth 文件、URL 或网络异常对象落日志。
  console.error("AUDIO_HOST_START_FAILED");
  process.exitCode = 1;
});
module.exports = { privateDirectory };
