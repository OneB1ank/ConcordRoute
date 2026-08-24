package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveCodexOverdraftProbeStateMigration(t *testing.T) {
	content, err := FS.ReadFile("274_remove_codex_overdraft_probe_state.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "- 'codex_quota_overdraft_probe'")
	require.Contains(t, sql, "- 'codex_5h_overdraft_started_at'")
	require.Contains(t, sql, "- 'codex_7d_overdraft_reset_at'")
	require.Contains(t, sql, `"codex_quota_overdraft"`)
	require.Contains(t, sql, "temp_unschedulable_until = CASE")
	require.NotContains(t, sql, "rate_limit_reset_at =")
	require.Contains(t, sql, "INSERT INTO scheduler_outbox")
	require.Contains(t, sql, "'account_changed'")
}
