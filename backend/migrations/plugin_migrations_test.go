package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 普通迁移由 runner 事务化；这里约束新增插件 DDL 的重复执行保护。
func TestPluginMigrationsReplayGuards(t *testing.T) {
	raw, err := FS.ReadFile("286_plugin_capability_routing.sql")
	require.NoError(t, err)
	sql := strings.ToUpper(string(raw))
	for _, name := range []string{"priority", "concurrency", "timeout", "ids"} {
		constraint := "SUB2API_PLUGIN_BINDINGS_" + strings.ToUpper(name) + "_CHECK"
		drop := strings.Index(sql, "DROP CONSTRAINT IF EXISTS "+constraint)
		add := strings.Index(sql, "ADD CONSTRAINT "+constraint)
		require.GreaterOrEqual(t, drop, 0)
		require.Greater(t, add, drop)
	}
	raw, err = FS.ReadFile("287_plugin_host_secret_grants.sql")
	require.NoError(t, err)
	require.Contains(t, strings.ToUpper(string(raw)), "CREATE INDEX IF NOT EXISTS IDX_SUB2API_PLUGIN_SECRET_GRANTS_EXPIRY")
}
