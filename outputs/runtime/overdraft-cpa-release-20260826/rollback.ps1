$ErrorActionPreference = 'Stop'
$repo = 'D:\c++\sub2api-tls-router-port\src'
Set-Location $repo
$files = @('backend/cmd/server/VERSION','backend/cmd/server/wire_gen.go','backend/internal/repository/account_repo_codex_overdraft.go','backend/internal/service/account_usage_service.go','backend/internal/service/openai_account_runtime_block_fastpath.go','backend/internal/service/openai_codex_probe_identity.go','backend/internal/service/openai_codex_quota_overdraft.go','backend/internal/service/openai_codex_quota_overdraft_probe.go','backend/internal/service/openai_codex_quota_overdraft_probe_test.go','backend/internal/service/openai_codex_quota_overdraft_test.go','backend/internal/service/openai_gateway_forward.go','backend/internal/service/openai_gateway_passthrough.go','backend/internal/service/openai_gateway_service.go','backend/internal/service/openai_gateway_service_test.go','backend/internal/service/openai_ws_v2_passthrough_adapter.go','backend/internal/service/openai_ws_v2_passthrough_lifecycle_test.go','backend/internal/service/wire.go')
foreach ($f in $files) { git checkout HEAD -- $f 2>$null }
Write-Output 'rollback complete'
