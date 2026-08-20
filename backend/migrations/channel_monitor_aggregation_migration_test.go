package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 迁移 253 必须对齐旧渠道监控迁移创建的表。CREATE TABLE IF NOT EXISTS
// 不会给已有表补列，因此兼容 ALTER 必须先于 deleted_at 索引执行。
func TestChannelMonitorAggregationMigrationReconcilesExistingRollupTable(t *testing.T) {
	content, err := FS.ReadFile("253_add_channel_monitor_aggregation.sql")
	require.NoError(t, err)
	sql := strings.ToUpper(string(content))

	historyColumn := strings.Index(sql, "ALTER TABLE CHANNEL_MONITOR_HISTORIES")
	historyIndex := strings.Index(sql, "IDX_CHANNEL_MONITOR_HISTORIES_DELETED_AT")
	rollupCreate := strings.Index(sql, "CREATE TABLE IF NOT EXISTS CHANNEL_MONITOR_DAILY_ROLLUPS")
	rollupColumn := strings.Index(sql, "ALTER TABLE CHANNEL_MONITOR_DAILY_ROLLUPS")
	rollupIndex := strings.Index(sql, "IDX_CHANNEL_MONITOR_DAILY_ROLLUPS_DELETED_AT")

	require.GreaterOrEqual(t, historyColumn, 0)
	require.Greater(t, historyIndex, historyColumn)
	require.GreaterOrEqual(t, rollupCreate, 0)
	require.Greater(t, rollupColumn, rollupCreate)
	require.Greater(t, rollupIndex, rollupColumn)
}
