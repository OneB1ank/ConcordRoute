# 验证插件扩展

以下测试只使用临时目录、测试容器、合成账号和本机上游，不需要真实 Provider 凭据。它们不会部署或启用生产插件。

## 核心网关、管理 API 和计费

需要 Go 工具链和正在运行的本机 Docker。从仓库 `backend` 目录执行：

```powershell
go test -tags plugin_e2e ./cmd/server -run '^TestPluginProductionE2E$' -count=1 -v -timeout=8m
```

测试装配生产依赖图和路由，创建独立 PostgreSQL/Redis、登录临时管理员、启用 TOTP、通过二次验证安装签名包。它核对：

- 请求的生成参数修改生效，模型和上游认证保留；
- 拒绝返回单个合法的 403 JSON，超时返回单个合法的 503 JSON；
- 两种错误均不调用上游、不扣费、不禁用账号；
- 热升级、回滚以及停用后的内置路径有效；
- 24 个请求以 8 并发执行，上游调用数、用量记录、费用和余额一致。

延迟输出只是该机器上合成上游的回归结果，不代表生产容量。测试的本机 HTTP 上游使用独立的测试配置，不改变生产 HTTPS/SSRF 规则。

## 真实浏览器管理流程

需要前端依赖已安装，以及 Python Playwright 和 Chromium。打开三个终端，按顺序执行。示例路径均基于仓库根目录，不要在输出中分享临时 fixture（它含合成登录凭据）。

终端一，`backend` 目录：

```powershell
New-Item -ItemType Directory -Force dist | Out-Null
$env:SUB2API_PLUGIN_E2E_FIXTURE = Join-Path (Get-Location) 'dist/plugin-e2e-fixture.json'
go test -tags plugin_e2e ./cmd/server -run '^TestPluginProductionE2E$' -count=1 -v -timeout=22m
```

等待输出 `Browser fixture ready`。测试最多等待浏览器 15 分钟，之后自动失败并清理容器。

终端二，`frontend` 目录：

```powershell
$pluginFixture = Get-Content ../backend/dist/plugin-e2e-fixture.json -Raw | ConvertFrom-Json
$env:VITE_DEV_PROXY_TARGET = $pluginFixture.backend_url
$env:VITE_DEV_PORT = '5187'
pnpm exec vite --host 127.0.0.1 --port 5187 --strictPort
```

终端三，`frontend` 目录：

```powershell
python scripts/plugin-e2e.py --fixture ../backend/dist/plugin-e2e-fixture.json --url http://127.0.0.1:5187 --output ../backend/dist/plugin-browser-evidence
```

浏览器执行新会话登录和 TOTP、上传、二次验证、启用、沙箱 iframe 配置与测试、作用域保存、升级、回滚和停用。成功后通知 Go 测试继续卸载、关闭进程和清理容器。浏览器总会关闭；结束后在终端二用 Ctrl+C 停止 Vite。截图和脱敏可访问树保存在输出目录。

## 旧协议与隔离回归

无需提供外部 v1 包即可验证原协议真实子进程：

```powershell
go test ./internal/service -run '^TestPluginV1ProcessCompatibility$' -count=1
go test ./internal/service -run '^TestPluginExtensionProcessIntegration$' -count=1
```

第一项只构建使用 v1 SDK 的测试程序，覆盖分块响应、取消及 `request_sent=true`。第二项覆盖 v2 签名包与双管理器恢复。容器运行器和 Linux 宿主验证见 [隔离说明](sandbox.md)；Host API 验证见 [Host API](host-api.md)。
