package repository

import (
	"context"
	"errors"
	"testing"

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
