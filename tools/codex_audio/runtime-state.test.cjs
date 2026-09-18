"use strict";
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { startAudioHost } = require("./audio-host.cjs");
const { loadBootstrap, runtimeRevision } = require("./runtime-state.cjs");

// 全部凭据均为本地随机夹具，不读取真实 auth.json 或调用云端接口。
function tempDirectory(t, prefix) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  t.after(() => {
    const resolved = fs.realpathSync(directory);
    assert.equal(path.dirname(resolved), fs.realpathSync(os.tmpdir()));
    assert.ok(path.basename(resolved).startsWith(prefix));
    // 夹具只包含平铺文件，逐个删除而非递归操作计算得到的目录。
    for (const entry of fs.readdirSync(resolved, { withFileTypes: true })) {
      assert.ok(entry.isFile());
      fs.unlinkSync(path.join(resolved, entry.name));
    }
    fs.rmdirSync(resolved);
  });
  return directory;
}
test("安装能力跨宿主重启稳定，短期上传能力仍轮换", async t => {
  const directory = tempDirectory(t, "concord-audio-state-");
  const bootstrap = loadBootstrap(directory);
  assert.equal(loadBootstrap(directory), bootstrap);
  assert.match(bootstrap, /^[A-Za-z0-9_-]{43}$/);
  const options = { gatewayOrigin: "https://fixture.invalid", apiKey: "SYNTHETIC_TEST_GATEWAY_KEY_123",
    bootstrap, fetchImpl: () => assert.fail("测试不得请求上游") };
  let previous;
  for (let i = 0; i < 2; i++) {
    const host = await startAudioHost(options);
    try {
      const response = await fetch(host.controlUrl + "/audio/session", { method: "POST",
        headers: { Authorization: "Bearer " + bootstrap } });
      assert.equal(response.status, 200);
      const connection = await response.json();
      if (previous) assert.notEqual(previous, connection.dictation.localCapability);
      previous = connection.dictation.localCapability;
    } finally { await host.close(); }
  }
});

test("损坏的能力文件保持原文并停止启动，拒绝弱凭据", async t => {
  const directory = tempDirectory(t, "concord-audio-corrupt-");
  fs.writeFileSync(path.join(directory, "bootstrap.secret"), "invalid");
  assert.throws(() => loadBootstrap(directory), /Invalid/);
  assert.equal(fs.readFileSync(path.join(directory, "bootstrap.secret"), "utf8"), "invalid");
  await assert.rejects(startAudioHost({ bootstrap: "weak" }), /bootstrap/);
});

test("运行版本仅哈希程序文件，测试/凭据/日志不影响版本", async t => {
  const directory = tempDirectory(t, "concord-audio-revision-");
  fs.writeFileSync(path.join(directory, "main.cjs"), "module.exports=1;");
  const revision = runtimeRevision(directory);
  fs.writeFileSync(path.join(directory, "bootstrap.secret"), "SYNTHETIC");
  fs.writeFileSync(path.join(directory, "unit.test.cjs"), "different test");
  fs.writeFileSync(path.join(directory, "host.stdout.log"), "different log");
  assert.equal(runtimeRevision(directory), revision);
  fs.writeFileSync(path.join(directory, "main.cjs"), "module.exports=2;");
  assert.notEqual(runtimeRevision(directory), revision);
});

test("宿主就绪与桌面加载独立，状态回执需要认证且不接受业务正文", async () => {
  const host = await startAudioHost({ gatewayOrigin: "https://fixture.invalid",
    apiKey: "SYNTHETIC_TEST_GATEWAY_KEY_123", revision: "abc123", liveEnabled: false });
  const headers = { Authorization: "Bearer " + host.bootstrap };
  const status = async () => (await fetch(host.controlUrl + "/audio/status", { headers })).json();
  try {
    let value = await status();
    assert.equal(value.adapter.state, "unconfirmed");
    assert.equal(value.liveEnabled, false);
    assert.equal(value.revision, "abc123");
    assert.equal((await fetch(host.controlUrl + "/audio/adapter/ready", { method: "POST" })).status, 401);
    assert.equal((await fetch(host.controlUrl + "/audio/adapter/ready", { method: "POST", headers, body: "private text" })).status, 413);
    assert.equal((await status()).adapter.state, "unconfirmed");
    assert.equal((await fetch(host.controlUrl + "/audio/adapter/ready", { method: "POST", headers })).status, 200);
    value = await status();
    assert.equal(value.adapter.state, "ready");
    assert.ok(value.adapter.reportedAt);
    assert.equal((await fetch(host.controlUrl + "/audio/adapter/disposed", { method: "POST", headers })).status, 200);
    assert.equal((await status()).adapter.state, "disposed");
  } finally { await host.close(); }
});
