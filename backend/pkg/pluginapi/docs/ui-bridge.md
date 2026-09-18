# UI Bridge v1

## 加载方式

宿主为每次打开配置页创建短时 UI 会话：

```text
/api/v1/plugin-ui/<asset-token>/index.html#bridge_token=<bridge-token>
```

资源 Token 用于读取包内 `ui/` 文件，Bridge Token 只存在于 URL fragment，不会发送到服务器。iframe 使用 `sandbox="allow-scripts"`，不授予 `allow-same-origin`。

桌面端须使用当前 API 后端地址解析这个相对 URL，不能相对于 Tauri 资源域名解析。资源响应的 CSP `frame-ancestors` 仅允许宿主同源及固定的 `tauri://localhost`、`http://tauri.localhost`、`https://tauri.localhost`；普通管理/API 页面的防嵌入策略不受影响。

宿主收到来源窗口、`null` origin 和 Bridge Token 均匹配的 `sub2api.plugin.ready` 后才认为配置页就绪。iframe 的 `load` 事件也可能来自错误页面，不能作为就绪信号；15 秒未就绪时显示加载失败与重试入口。关闭或切换配置会话后，宿主丢弃旧会话迟到的请求结果。

UI 只能加载包内、已在清单声明的资源。CSP 禁止外部网络连接、表单提交和外部 frame。

## 消息信封

UI 到宿主：

```json
{
  "source": "sub2api-plugin-ui",
  "bridge_token": "TOKEN",
  "type": "config.load",
  "request_id": "UNIQUE_ID"
}
```

宿主到 UI：

```json
{
  "source": "sub2api-plugin-host",
  "bridge_token": "TOKEN",
  "request_id": "UNIQUE_ID",
  "ok": true
}
```

## 方法

| `type` | UI 参数 | 成功响应 |
|---|---|---|
| `sub2api.plugin.ready` | 无 | 无响应 |
| `config.load` | 无 | `config` |
| `config.save` | `config` 对象 | 规范化后的 `config` |
| `config.test` | 无 | `result` |
| `ui.resize` | `height` | 无响应 |
| `ui.notify` | `level`、`message` | 无响应 |

`config.test` 在 v1 中测试已保存配置。UI 若要测试当前表单，应先调用 `config.save`。

## 必须执行的校验

UI 接收消息时必须验证 `event.source === parent`、消息来源标识、Bridge Token 和等待中的 `request_id`。每个请求必须有超时和卸载清理。

宿主不会向 iframe 提供管理员 Token。插件 UI 不得尝试访问管理 API、Cookie、父页面 DOM 或浏览器存储中的宿主数据。
