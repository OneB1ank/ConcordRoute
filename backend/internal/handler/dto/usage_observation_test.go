package dto

import (
	"encoding/json"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 两个整数只出现在管理接口，不向普通用户泄露上游观测元数据。
func TestUsageObservationAdminOnly(t *testing.T) {
	status, size := 200, 332
	mode := service.CodexTurnStateRequestModeInjected
	log := &service.UsageLog{UpstreamStatusCode: &status, CodexTurnStateBytes: &size, CodexTurnStateRequestMode: &mode}
	admin, err := json.Marshal(UsageLogFromServiceAdmin(log))
	require.NoError(t, err)
	require.Contains(t, string(admin), `"upstream_status_code":200`)
	require.Contains(t, string(admin), `"codex_turn_state_bytes":332`)
	require.Contains(t, string(admin), `"codex_turn_state_request_mode":"injected"`)
	user, err := json.Marshal(UsageLogFromService(log))
	require.NoError(t, err)
	require.NotContains(t, string(user), "upstream_status_code")
	require.NotContains(t, string(user), "codex_turn_state_bytes")
	require.NotContains(t, string(user), "codex_turn_state_request_mode")
	legacy, err := json.Marshal(UsageLogFromServiceAdmin(&service.UsageLog{}))
	require.NoError(t, err)
	require.NotContains(t, string(legacy), "upstream_status_code")
	require.NotContains(t, string(legacy), "codex_turn_state_bytes")
	require.NotContains(t, string(legacy), "codex_turn_state_request_mode")
}
