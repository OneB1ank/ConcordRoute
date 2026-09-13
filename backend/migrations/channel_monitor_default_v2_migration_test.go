package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 渠道监控默认必须落到 V2，避免升级后继续执行主动探测。
func TestChannelMonitorDefaultV2Migration(t *testing.T) {
	content, err := FS.ReadFile("277_channel_monitor_default_v2.sql")
	require.NoError(t, err)

	sql := strings.ToLower(strings.Join(strings.Fields(string(content)), " "))
	require.Contains(t, sql, "insert into settings as target (key, value)")
	require.Contains(t, sql, "values ('channel_monitor_mode', 'v2')")
	require.Contains(t, sql, "on conflict (key) do update")
	require.Contains(t, sql, "set value = case")
	require.Contains(t, sql, "then 'v2'")
	require.Contains(t, sql, "262_channel_monitor_mode.sql")
	require.Contains(t, sql, "target.updated_at = legacy.applied_at")
}
