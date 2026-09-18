const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { readFileSync } = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const installer = path.join(__dirname, "install.ps1");

test("installer has no network origin default", () => {
  const source = readFileSync(installer, "utf8");
  assert.match(source, /\[string\]\$GatewayOrigin\s*=\s*""/);
  assert.doesNotMatch(source, /\[string\]\$GatewayOrigin\s*=\s*"https:\/\//);
});

test("install fails before setup when GatewayOrigin is omitted", { skip: process.platform !== "win32" }, () => {
  const result = spawnSync("pwsh.exe", ["-NoLogo", "-NoProfile", "-File", installer, "-Mode", "Install"], {
    encoding: "utf8",
    windowsHide: true,
  });
  assert.notEqual(result.status, 0);
  assert.match(`${result.stdout}\n${result.stderr}`, /GatewayOrigin/);
  assert.doesNotMatch(`${result.stdout}\n${result.stderr}`, /DESKTOP_CONTRACT_VERIFIED|AUDIO_HOST_RUNNING/);
});
