package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBulkUpdateEnsuresCodexFingerprintSeedWithPerRowSQL(t *testing.T) {
	exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
	repo := newAccountRepositoryWithSQL(nil, exec, nil)

	_, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{
		Extra: map[string]any{
			"codex_fingerprint_mode": "cockpit",
			"codex_fingerprint_seed": "22222222-2222-4222-8222-222222222222",
		},
		EnsureCodexFingerprintSeed: true,
	})

	require.NoError(t, err)
	require.NotEmpty(t, exec.execQueries)
	query := normalizeSQLWhitespace(exec.execQueries[0])
	require.Contains(t, query, "jsonb_set")
	require.Contains(t, query, "gen_random_uuid()::text")
	require.Contains(t, query, "platform = 'openai' AND type = 'oauth'")
	for _, pattern := range codexFingerprintSeedAcceptedPatterns {
		require.Contains(t, query, pattern)
	}
	require.Equal(t, len(codexFingerprintSeedAcceptedPatterns), strings.Count(query, " ~* '"))
	require.Contains(t, query, "regexp_replace(LOWER(")
	require.Contains(t, query, codexFingerprintNilSeedHex)
	require.NotContains(t, query, "22222222-2222-4222-8222-222222222222")
	require.NotEmpty(t, exec.execArgs)
	payload, ok := exec.execArgs[0][0].([]byte)
	require.True(t, ok)
	require.JSONEq(t, `{"codex_fingerprint_mode":"cockpit"}`, string(payload))
}
