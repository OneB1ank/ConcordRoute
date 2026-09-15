package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetCodexIdentityBindings_SelectsOnlyIDAndExtra(t *testing.T) {
	for _, kind := range []string{"found", "missing", "storage_error"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			// 显式验证 mock 连接关闭，避免遗漏清理错误。
			t.Cleanup(func() {
				mock.ExpectClose()
				require.NoError(t, db.Close())
			})
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			repo := newAccountRepositoryWithSQL(client, nil, nil)
			// 只允许这一条窄查询，关联查询或凭据读取会使 mock 失败。
			query := mock.ExpectQuery(`^SELECT "accounts"\."id", "accounts"\."extra" FROM "accounts" WHERE `).
				WithArgs(int64(701))
			storageErr := errors.New("storage error")
			switch kind {
			case "found":
				query.WillReturnRows(sqlmock.NewRows([]string{"id", "extra"}).
					AddRow(701, []byte(`{"codex_identity_bindings_v1":{},"codex_turn_lineage_bindings_v1":{}}`)))
			case "missing":
				query.WillReturnRows(sqlmock.NewRows([]string{"id", "extra"}))
			default:
				query.WillReturnError(storageErr)
			}
			account, err := repo.GetCodexIdentityBindings(context.Background(), 701)
			switch kind {
			case "found":
				require.NoError(t, err)
				require.EqualValues(t, 701, account.ID)
				require.Contains(t, account.Extra, service.CodexIdentityBindingsExtraKey)
				require.Contains(t, account.Extra, service.CodexTurnLineageBindingsExtraKey)
				require.Nil(t, account.Credentials)
				require.Nil(t, account.Proxy)
				require.Empty(t, account.Groups)
			case "missing":
				require.ErrorIs(t, err, service.ErrAccountNotFound)
			default:
				require.ErrorIs(t, err, storageErr)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWithCodexIdentityBindings_HoldsRowLockAcrossPreparation(t *testing.T) {
	for _, mode := range []string{"write", "read_only", "prepare_error"} {
		t.Run(mode, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() {
				mock.ExpectClose()
				require.NoError(t, db.Close())
			})
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			repo := newAccountRepositoryWithSQL(client, nil, nil)
			mock.ExpectBegin()
			mock.ExpectQuery(`(?s)SELECT id, COALESCE\(extra, '\{\}'::jsonb\).*FOR UPDATE`).
				WithArgs(int64(702)).
				WillReturnRows(sqlmock.NewRows([]string{"id", "extra"}).
					AddRow(702, []byte(`{"codex_identity_bindings_v1":{"seed":{"uuid":"01993000-0000-7000-8000-000000000001"}}}`)))
			prepareErr := errors.New("synthetic prepare error")
			switch mode {
			case "write":
				mock.ExpectExec(`(?s)UPDATE accounts.*SET extra = .*WHERE id = \$2 AND deleted_at IS NULL`).
					WithArgs(sqlmock.AnyArg(), int64(702)).
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			case "read_only":
				mock.ExpectCommit()
			default:
				mock.ExpectRollback()
			}

			err = repo.WithCodexIdentityBindings(context.Background(), 702, func(latest *service.Account) (map[string]any, error) {
				require.EqualValues(t, 702, latest.ID)
				require.Contains(t, latest.Extra, service.CodexIdentityBindingsExtraKey)
				switch mode {
				case "write":
					return map[string]any{service.CodexIdentityBindingsExtraKey: map[string]any{"seed": "01993000-0000-7000-8000-000000000001"}}, nil
				case "prepare_error":
					return nil, prepareErr
				default:
					return nil, nil
				}
			})
			if mode == "prepare_error" {
				require.ErrorIs(t, err, prepareErr)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestWithCodexIdentityBindings_StopsOnContextDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		require.NoError(t, mock.ExpectationsWereMet())
	})
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newAccountRepositoryWithSQL(client, nil, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT id, COALESCE\(extra, '\{\}'::jsonb\).*FOR UPDATE`).
		WithArgs(int64(703)).
		WillDelayFor(100 * time.Millisecond).
		WillReturnRows(sqlmock.NewRows([]string{"id", "extra"}).AddRow(703, []byte(`{}`)))
	// 取消事务的后台回滚可能晚于函数返回；关闭期望必须在请求开始前登记，
	// 避免清理阶段与 sqlmock 驱动的异步 Close 并发修改期望队列。
	mock.ExpectClose()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err = repo.WithCodexIdentityBindings(ctx, 703, func(_ *service.Account) (map[string]any, error) {
		t.Fatal("数据库行锁读取超时后不得进入绑定生成回调")
		return nil, nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	// database/sql 在 context 取消后异步回滚事务；等连接归还后再进入 Cleanup，
	// 使 db.Close 确实触发已登记的驱动关闭期望。
	require.Eventually(t, func() bool { return db.Stats().InUse == 0 }, time.Second, time.Millisecond)
}
