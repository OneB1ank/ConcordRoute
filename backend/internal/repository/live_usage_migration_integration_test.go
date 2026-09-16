//go:build integration

package repository

import (
	"context"
	"testing"

	dbmigrations "github.com/TokenFlux/TokenRouter/migrations"
	"github.com/stretchr/testify/require"
)

// 复现线上旧迁移后补执行使约束收窄，验证前向修复及重复执行均不改动已有数据。
func TestMigration279RestoresLiveAfterLegacyConstraint(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	_, err := tx.ExecContext(ctx, `CREATE TEMP TABLE usage_logs (request_type SMALLINT NOT NULL) ON COMMIT DROP;
INSERT INTO usage_logs SELECT generate_series(0,4);`)
	require.NoError(t, err)
	for _, name := range []string{"218_allow_live_usage_request_type.sql", "195_allow_cyber_blocked_usage_request_type.sql"} {
		content, readErr := dbmigrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		_, err = tx.ExecContext(ctx, string(content))
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, "SAVEPOINT before_live")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "INSERT INTO usage_logs VALUES (5)")
	require.ErrorContains(t, err, "usage_logs_request_type_check")
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_live")
	require.NoError(t, err)

	content, err := dbmigrations.FS.ReadFile("279_restore_live_usage_request_type.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(content))
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO usage_logs VALUES (5)")
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM usage_logs").Scan(&count))
	require.Equal(t, 6, count)
	// 放宽只到Live=5，未知类型与负值仍由数据库拒绝。
	for _, invalid := range []int{-1, 6} {
		_, err = tx.ExecContext(ctx, "SAVEPOINT invalid_type")
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, "INSERT INTO usage_logs VALUES ($1)", invalid)
		require.ErrorContains(t, err, "usage_logs_request_type_check")
		_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT invalid_type")
		require.NoError(t, err)
	}
}
