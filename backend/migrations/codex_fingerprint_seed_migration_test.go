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

func TestMigration249RepairsInvalidCodexFingerprintSeeds(t *testing.T) {
	content, err := FS.ReadFile("249_repair_codex_fingerprint_seeds.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "gen_random_uuid()")
	require.Contains(t, sql, "^[0-9a-f]{32}$")
	require.Contains(t, sql, "^urn:uuid:")
	require.Contains(t, sql, "00000000000000000000000000000000")
	require.Contains(t, sql, "child.parent_account_id = parent.id")
	require.Contains(t, sql, "IS DISTINCT FROM parent.extra->>'codex_fingerprint_seed'")
}
