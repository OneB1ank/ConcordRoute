package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 观测迁移保持可滚动回退，不回填、建索引、存原文或重写大表。
func TestUsageUpstreamObservationMigration(t *testing.T) {
	content, err := FS.ReadFile("281_usage_upstream_observation.sql")
	require.NoError(t, err)
	sql := strings.ToUpper(string(content))
	require.Contains(t, sql, "SET LOCAL LOCK_TIMEOUT = '5S'")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS UPSTREAM_STATUS_CODE SMALLINT")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS CODEX_TURN_STATE_BYTES INTEGER")
	for _, forbidden := range []string{"CREATE INDEX", "UPDATE USAGE_LOGS", "NOT NULL", "DEFAULT ", "DROP COLUMN"} {
		require.NotContains(t, sql, forbidden)
	}
}
