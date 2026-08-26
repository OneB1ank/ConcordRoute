param(
    [string]$Repo = 'D:\c++\sub2api-tls-router-port\src'
)

$ErrorActionPreference = 'Stop'

$baseline = '7bf1db1294ef272d0f75d35960a8720822dddec5'
$tracked = @(
    'backend/cmd/server/VERSION',
    'backend/cmd/server/wire_gen.go',
    'backend/internal/service/account_usage_service.go',
    'backend/internal/service/openai_account_runtime_block_fastpath.go',
    'backend/internal/service/openai_codex_probe_identity.go',
    'backend/internal/service/openai_codex_quota_overdraft.go',
    'backend/internal/service/openai_codex_quota_overdraft_test.go',
    'backend/internal/service/openai_gateway_forward.go',
    'backend/internal/service/openai_gateway_passthrough.go',
    'backend/internal/service/openai_gateway_service.go',
    'backend/internal/service/openai_gateway_service_test.go',
    'backend/internal/service/gateway_service.go',
    'backend/internal/service/openai_ws_v2_passthrough_adapter.go',
    'backend/internal/service/openai_ws_v2_passthrough_lifecycle_test.go',
    'backend/internal/service/wire.go'
)
$added = @(
    'backend/internal/repository/account_repo_codex_overdraft.go',
    'backend/internal/service/openai_codex_quota_overdraft_probe.go',
    'backend/internal/service/openai_codex_quota_overdraft_probe_test.go'
)

Set-Location -LiteralPath $Repo
git cat-file -e "$baseline^{commit}"
git checkout $baseline -- $tracked
foreach ($path in $added) {
    if (Test-Path -LiteralPath $path) {
        Remove-Item -LiteralPath $path -Force
    }
}
Write-Output "rollback complete: restored CPA overdraft changes to $baseline"
