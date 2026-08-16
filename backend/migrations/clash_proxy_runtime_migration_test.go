package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration248CreatesIsolatedClashProxyRuntimeTables(t *testing.T) {
	content, err := FS.ReadFile("248_clash_proxy_runtime.sql")
	require.NoError(t, err)

	sql := string(content)
	for _, table := range []string{
		"clash_proxy_nodes",
		"clash_proxy_profiles",
		"clash_proxy_profile_nodes",
		"clash_proxy_runtime_instances",
		"clash_proxy_account_bindings",
	} {
		require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS "+table)
	}
	require.Contains(t, sql, "managed_proxy_id BIGINT UNIQUE REFERENCES proxies(id)")
	require.Contains(t, sql, "account_id        BIGINT NOT NULL UNIQUE REFERENCES accounts(id)")
	require.Contains(t, sql, "previous_proxy_id BIGINT REFERENCES proxies(id) ON DELETE SET NULL")
}
