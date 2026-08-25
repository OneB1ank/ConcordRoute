package repository

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/TokenFlux/TokenRouter/ent"
	_ "github.com/TokenFlux/TokenRouter/ent/runtime"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

type captureEntQueryMatcher struct {
	actual *string
}

func (m captureEntQueryMatcher) Match(_, actual string) error {
	if m.actual == nil {
		return fmt.Errorf("query capture target is nil")
	}
	*m.actual = actual
	return nil
}

func TestListSchedulableAccountLoadsUsesSingleProjectionQuery(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("schedulable account load projection").
		WillReturnRows(sqlmock.NewRows([]string{"id", "concurrency", "load_factor"}).
			AddRow(int64(11), 3, nil).
			AddRow(int64(12), 2, 7))

	loads, err := repo.ListSchedulableAccountLoads(context.Background())
	require.NoError(t, err)
	require.Len(t, loads, 2)
	require.Equal(t, int64(11), loads[0].ID)
	require.Equal(t, 3, loads[0].MaxConcurrency)
	require.Equal(t, int64(12), loads[1].ID)
	require.Equal(t, 7, loads[1].MaxConcurrency)
	require.NoError(t, mock.ExpectationsWereMet(), "projection path must execute exactly one query")

	normalized := normalizeSQLWhitespace(capturedSQL)
	selectClause, _, found := strings.Cut(normalized, " FROM ")
	require.True(t, found, "unexpected projection SQL: %s", normalized)
	require.Equal(t, 2, strings.Count(selectClause, ","), "projection must select exactly three columns: %s", selectClause)
	require.Contains(t, selectClause, `"id"`)
	require.Contains(t, selectClause, `"concurrency"`)
	require.Contains(t, selectClause, `"load_factor"`)
	require.NotContains(t, selectClause, "credentials")
	require.NotContains(t, selectClause, "extra")
	require.NotContains(t, selectClause, "proxy_id")
	require.NotContains(t, normalized, "account_groups")
	require.NotContains(t, normalized, "proxies")
	for _, predicateColumn := range []string{
		"status",
		"schedulable",
		"temp_unschedulable_until",
		"expires_at",
		"auto_pause_on_expired",
		"overload_until",
		"rate_limit_reset_at",
		"deleted_at",
	} {
		require.Contains(t, normalized, predicateColumn)
	}
	_, orderClause, hasOrder := strings.Cut(normalized, " ORDER BY ")
	require.True(t, hasOrder, "projection query must preserve schedulable account order: %s", normalized)
	require.Contains(t, orderClause, `"priority" ASC`)
}

func TestSchedulableAccountQueryScopesCodexQuotaOverdraftToMarkedContext(t *testing.T) {
	buildQuery := func(ctx context.Context) string {
		var capturedSQL string
		db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		driver := entsql.OpenDB(dialect.Postgres, db)
		client := dbent.NewClient(dbent.Driver(driver))
		t.Cleanup(func() { _ = client.Close() })
		repo := newAccountRepositoryWithSQL(client, db, nil)
		mock.ExpectQuery("schedulable query").WillReturnRows(sqlmock.NewRows([]string{"id", "concurrency", "load_factor"}))
		_, err = repo.ListSchedulableAccountLoads(ctx)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
		return normalizeSQLWhitespace(capturedSQL)
	}

	ordinarySQL := buildQuery(context.Background())
	require.NotContains(t, ordinarySQL, `"temp_unschedulable_reason" LIKE`)

	overdraftSQL := buildQuery(service.WithCodexQuotaOverdraftScheduling(context.Background()))
	require.Contains(t, overdraftSQL, `"temp_unschedulable_reason" LIKE`)
	require.Contains(t, overdraftSQL, `"platform" =`)
	require.Contains(t, overdraftSQL, `"type" =`)
	require.Contains(t, overdraftSQL, `"parent_account_id" IS NULL`)
	require.Contains(t, overdraftSQL, `"extra"`)
	require.Contains(t, overdraftSQL, `"extra" ->> $4`)
	require.Contains(t, overdraftSQL, `::double precision`)
	require.Contains(t, overdraftSQL, `>=`)
	require.GreaterOrEqual(t, strings.Count(overdraftSQL, `"extra" ->>`), 5, "开关与 5h/7d 准备门都必须进入 SQL 预筛选")
	require.Equal(t, 2, strings.Count(overdraftSQL, `"temp_unschedulable_reason" LIKE`), "threshold and legacy probe pause sources must both be eligible")
	require.NotContains(t, overdraftSQL, `codex_quota_overdraft_probe,status`)
	require.NotContains(t, overdraftSQL, `codex_quota_overdraft_probe,reason_code`)
	require.NotContains(t, overdraftSQL, `?`, "PostgreSQL query must not retain Ent's generic placeholder")
	require.NotContains(t, overdraftSQL, `::boolean`, "畸形历史值不得让可调度查询因布尔强转失败")
}

func TestDiscardDeprecatedAccountExtraRemovesLegacyCodexOverdraftProbe(t *testing.T) {
	extra := map[string]any{
		deprecatedCodexQuotaOverdraftProbeExtraKey: map[string]any{"status": "passed"},
		"custom": "value",
	}

	discardDeprecatedAccountExtra(extra)

	require.NotContains(t, extra, deprecatedCodexQuotaOverdraftProbeExtraKey)
	require.Equal(t, "value", extra["custom"])
}

func TestListSchedulableByGroupIDAndPlatformUsesPostgresOverdraftPlaceholder(t *testing.T) {
	var capturedSQL string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(captureEntQueryMatcher{actual: &capturedSQL}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectQuery("group snapshot account query").
		WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "created_at"}))
	accounts, err := repo.ListSchedulableByGroupIDAndPlatform(
		service.WithCodexQuotaOverdraftScheduling(context.Background()),
		2,
		service.PlatformOpenAI,
	)
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, mock.ExpectationsWereMet())

	normalized := normalizeSQLWhitespace(capturedSQL)
	require.Contains(t, normalized, `"temp_unschedulable_reason" LIKE`)
	require.Regexp(t, `"extra" ->> \$[0-9]+`, normalized)
	require.NotContains(t, normalized, `?`, "snapshot rebuild SQL must be valid PostgreSQL")
}
