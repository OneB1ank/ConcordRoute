"use strict";

// 共享纯函数不引入 Node API，桌面侧可由宿主打包后加载。
function validMultipart(value) {
  return typeof value === "string" && value.length <= 200
    && /^multipart\/form-data;\s*boundary=(?:[A-Za-z0-9_-]{1,70}|"[A-Za-z0-9_'()+,./:=?-]{1,70}")$/i.test(value);
}

module.exports = { validMultipart };
