package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration250PersistsAndBackfillsClashProfileAutoStart(t *testing.T) {
	content, err := FS.ReadFile("250_clash_proxy_profile_autostart.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS auto_start BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "runtime.status IN ('starting', 'running')")
	require.Contains(t, sql, "profile.managed_proxy_id IS NOT NULL")
	require.Contains(t, sql, "binding.enabled = TRUE")
	require.Contains(t, sql, "profile.status = 'active'")
	require.Contains(t, sql, "WHERE deleted_at IS NULL AND status = 'active' AND auto_start = TRUE")
}
