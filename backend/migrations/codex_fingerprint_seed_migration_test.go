package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration247BackfillsPersistentCodexFingerprintSeeds(t *testing.T) {
	content, err := FS.ReadFile("247_backfill_codex_fingerprint_seed.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "gen_random_uuid()")
	require.Contains(t, sql, "'{codex_fingerprint_seed}'")
	require.Contains(t, sql, "parent_account_id IS NULL")
	require.Contains(t, sql, "child.parent_account_id = parent.id")
	require.Contains(t, sql, "parent.extra->>'codex_fingerprint_seed'")
	require.Contains(t, sql, "NULLIF(BTRIM(COALESCE(extra->>'codex_fingerprint_seed', '')), '') IS NULL")
}
