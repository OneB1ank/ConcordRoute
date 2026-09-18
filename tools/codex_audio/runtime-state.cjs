"use strict";
const fs = require("node:fs");
const path = require("node:path");
const { randomBytes, createHash } = require("node:crypto");

// 调用方先收紧目录 ACL；安装能力跨进程保留，上传能力仍由 helper 短期签发。
function loadBootstrap(directory) {
  const file = path.join(directory, "bootstrap.secret");
  try { fs.writeFileSync(file, randomBytes(32).toString("base64url"), { flag: "wx", mode: 0o600 }); }
  catch (error) { if (error.code !== "EEXIST") throw error; }
  const value = fs.readFileSync(file, "utf8");
  if (!/^[A-Za-z0-9_-]{43}$/.test(value)) throw new Error("Invalid audio bootstrap file");
  return value;
}

function runtimeRevision(directory) {
  const hash = createHash("sha256");
  const files = fs.readdirSync(directory).filter(name =>
    (name.endsWith(".cjs") && !name.endsWith(".test.cjs")) ||
    ["install.ps1", "windows-speech.ps1", "config-patch.py"].includes(name)).sort();
  for (const name of files) hash.update(name + "\0").update(fs.readFileSync(path.join(directory, name)));
  return hash.digest("hex");
}

module.exports = { loadBootstrap, runtimeRevision };
