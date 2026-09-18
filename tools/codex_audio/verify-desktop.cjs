"use strict";
const fs = require("node:fs"), crypto = require("node:crypto");
const { CONTRACT } = require("./build-user-script.cjs");
// 只读取固定资源并核验已审查的哈希；不反编译、不改写应用安装包。
function verifyDesktopArchive(archive) {
  const descriptor = fs.openSync(archive, "r");
  try {
    const sizes = Buffer.alloc(16);
    fs.readSync(descriptor, sizes, 0, sizes.length, 0);
    const length = sizes.readUInt32LE(12);
    if (length > 16 * 1024 * 1024) throw new Error("Invalid archive header");
    const header = Buffer.alloc(length);
    fs.readSync(descriptor, header, 0, length, 16);
    let entry = JSON.parse(header);
    for (const name of ["webview", "assets", CONTRACT.asset]) entry = entry.files[name];
    if (!Number.isSafeInteger(entry.size) || entry.size > 64 * 1024 * 1024) throw new Error("Invalid desktop asset");
    const data = Buffer.alloc(entry.size);
    fs.readSync(descriptor, data, 0, data.length, 8 + sizes.readUInt32LE(4) + Number(entry.offset));
    if (crypto.createHash("sha256").update(data).digest("hex") !== CONTRACT.sha256) throw new Error("Unverified desktop build");
  } finally { fs.closeSync(descriptor); }
}
module.exports = { verifyDesktopArchive };
