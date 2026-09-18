"use strict";

const { startDictationHelper } = require("./dictation-helper.cjs");

// 仅由可信 Node 宿主 fork；密钥走父子进程 IPC，不接受 URL、命令行或公开配对接口。
if (!process.send) {
  process.stderr.write("Start this module from a trusted IPC host.\n");
  process.exitCode = 2;
} else {
  let helper;
  let stopping = false;
  let initializing = false;
  async function stop() {
    stopping = true;
    await helper?.close();
    if (process.connected) process.disconnect();
  }
  const startupTimer = setTimeout(() => { void stop(); }, 30000);
  startupTimer.unref();
  process.on("disconnect", () => { void stop(); });
  process.on("message", async message => {
    if (message?.type === "stop") { await stop(); return; }
    if (stopping || initializing || message?.type !== "start") return;
    initializing = true;
    clearTimeout(startupTimer);
    try {
      const { gatewayOrigin, apiKey, allowedOrigins } = message;
      helper = await startDictationHelper({ gatewayOrigin, apiKey, allowedOrigins });
      // 初始化过程中宿主退出，也立即撤销能力并关闭端口。
      if (stopping || !process.connected) { await stop(); return; }
      process.send({ type: "ready", connection: helper.connection }, error => {
        if (error) void stop();
      });
    } catch {
      // 不输出包含配置值的异常文本。
      process.exitCode = 1;
      await stop();
    }
  });
}
