"use strict";
const path = require("node:path");
const { spawn } = require("node:child_process");

// 不把文本拼到命令行；受预算约束的单次本机合成，取消即结束子进程。
function synthesizeSpeech(text, signal) {
  if (process.platform !== "win32" || typeof text !== "string" || !text.trim() || text.length > 400) return Promise.reject(new Error("speech_input_rejected"));
  return new Promise((resolve, reject) => {
    signal.throwIfAborted();
    // PS5 以 UTF-8 显式读取固定脚本，避免本机 ANSI 编码解释中文注释/字符串。
    const script = path.join(__dirname, "windows-speech.ps1");
    const shell = path.join(process.env.SystemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe");
    const env = { CONCORD_AUDIO_SPEECH_SCRIPT: script };
    for (const name of ["SystemRoot", "WINDIR", "TEMP", "TMP", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "ProgramData"]) {
      if (process.env[name]) env[name] = process.env[name];
    }
    const proc = spawn(shell, ["-NoProfile", "-NonInteractive", "-Command",
      "& ([ScriptBlock]::Create([IO.File]::ReadAllText($env:CONCORD_AUDIO_SPEECH_SCRIPT)))"], {
      windowsHide: true, env, stdio: ["pipe", "pipe", "pipe"],
    });
    const chunks = []; let length = 0, failure = null;
    const abort = () => { failure = new Error("speech_cancelled"); proc.kill(); };
    const timer = setTimeout(abort, 20000); timer.unref();
    signal.addEventListener("abort", abort, { once: true });
    proc.stdout.on("data", chunk => {
      length += chunk.length;
      if (length > 8 * 1024 * 1024) { failure = new Error("speech_too_large"); proc.kill(); }
      else chunks.push(chunk);
    });
    proc.stderr.on("data", () => {}); // 合成异常可能带原文，刻意不记录。
    proc.stdin.on("error", () => {});
    proc.once("error", () => { failure = new Error("speech_engine_unavailable"); });
    proc.once("close", code => {
      clearTimeout(timer); signal.removeEventListener("abort", abort);
      const result = Buffer.concat(chunks);
      if (failure || signal.aborted || code !== 0 || result.toString("ascii", 0, 4) !== "RIFF") reject(failure || new Error("speech_engine_failed"));
      else resolve(result);
    });
    proc.stdin.end(JSON.stringify({ text }));
    if (signal.aborted) abort();
  });
}
module.exports = { synthesizeSpeech };
