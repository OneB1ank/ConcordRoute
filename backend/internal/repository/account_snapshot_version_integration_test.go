//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 真实 PostgreSQL 中验证同事务和逆序事务的账号行版本；不读取业务账号。
func TestAccountSnapshotVersionMonotonic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO accounts (name, platform, type) VALUES ('snapshot-version-test', 'openai', 'oauth') RETURNING id`).Scan(&id))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id = $1", id)
	})

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback()
	var first, second, third time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `UPDATE accounts SET updated_at = '2090-01-01T00:00:00Z' WHERE id = $1 RETURNING updated_at`, id).Scan(&first))
	require.NoError(t, tx.QueryRowContext(ctx, `UPDATE accounts SET updated_at = NOW(), extra = '{"first":true}' WHERE id = $1 RETURNING updated_at`, id).Scan(&second))
	require.NoError(t, tx.QueryRowContext(ctx, `UPDATE accounts SET updated_at = NOW(), extra = '{"second":true}' WHERE id = $1 RETURNING updated_at`, id).Scan(&third))
	require.True(t, second.After(first))
	require.True(t, third.After(second))
	require.NoError(t, tx.Rollback())

	// 旧事务先开始但晚更新，行版本仍必须晚于另一个已完成的新事务。
	oldTx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer oldTx.Rollback()
	var started time.Time
	require.NoError(t, oldTx.QueryRowContext(ctx, "SELECT NOW()").Scan(&started))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `UPDATE accounts SET updated_at = NOW() WHERE id = $1 RETURNING updated_at`, id).Scan(&first))
	require.NoError(t, oldTx.QueryRowContext(ctx, `UPDATE accounts SET updated_at = NOW() WHERE id = $1 RETURNING updated_at`, id).Scan(&second))
	require.True(t, second.After(first))
	t.Log("same_transaction_monotonic=true older_transaction_late_update_monotonic=true")
}

// 实际 Redis 验证脚本版本比较及删除墓碑，避免只依赖 miniredis 的解释器。
func TestSchedulerAccountVersionRealRedis(t *testing.T) {
	ctx := context.Background()
	cache := NewSchedulerCache(testRedis(t))
	old := service.Account{ID: 998301, Name: "old", UpdatedAt: time.Now().UTC()}
	fresh := old
	fresh.Name = "fresh"
	fresh.UpdatedAt = old.UpdatedAt.Add(time.Microsecond)
	fresh.Extra = map[string]any{service.CodexIdentityBindingsExtraKey: map[string]any{"kept": "test-binding"}}
	require.NoError(t, cache.SetAccount(ctx, &fresh))
	require.NoError(t, cache.SetAccount(ctx, &old))
	got, err := cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "fresh", got.Name)
	require.Equal(t, fresh.Extra, got.Extra)
	require.NoError(t, cache.DeleteAccount(ctx, old.ID))
	require.NoError(t, cache.SetAccount(ctx, &fresh))
	got, err = cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Nil(t, got)
	t.Log("real_redis_stale_write_rejected=true deleted_account_not_resurrected=true")
}
