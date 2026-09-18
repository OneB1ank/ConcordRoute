package repository

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 覆盖 NULL、HTTP 200 携带 state 长度及其它状态码，使用 database/sql 真正执行 Scan 类型转换。
func TestUsageLogObservationSQLRoundTrip(t *testing.T) {
	for _, code := range []int{0, 200, 201, 202, 299} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			log := &service.UsageLog{UserID: 1, APIKeyID: 2, AccountID: 3, RequestID: "req-observation",
				Model: "test", CreatedAt: time.Now().UTC(), InputTokens: 8, OutputTokens: 2}
			stateBytes := 0
			if code != 0 {
				log.UpstreamStatusCode = &code
				log.CodexTurnStateBytes = &stateBytes
			}
			if code == 200 {
				stateBytes = 356
			}
			prepared := prepareUsageLogInsert(log)
			columns := strings.Split(usageLogSelectColumns, ", ")
			require.Len(t, columns, len(prepared.args)+1)
			require.Len(t, prepared.args, len(usageLogInsertArgTypes))
			values := []driver.Value{int64(42)}
			for _, arg := range prepared.args {
				value, err := driver.DefaultParameterConverter.ConvertValue(arg)
				require.NoError(t, err)
				values = append(values, value)
			}
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectQuery("SELECT observation_fixture").WillReturnRows(sqlmock.NewRows(columns).AddRow(values...))
			got, err := scanUsageLog(db.QueryRowContext(context.Background(), "SELECT observation_fixture"))
			require.NoError(t, err)
			require.Equal(t, log.UpstreamStatusCode, got.UpstreamStatusCode)
			require.Equal(t, log.CodexTurnStateBytes, got.CodexTurnStateBytes)
			require.Equal(t, 8, got.InputTokens)
			require.NoError(t, mock.ExpectationsWereMet())

			key := usageLogBatchKey(log.RequestID, log.APIKeyID)
			batch, args := buildUsageLogBatchInsertQuery([]string{key}, map[string]usageLogInsertPrepared{key: prepared})
			bestEffort, bestArgs := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
			require.Len(t, args, len(prepared.args)+1)
			require.Len(t, bestArgs, len(prepared.args))
			for _, field := range []string{"upstream_status_code", "codex_turn_state_bytes"} {
				require.GreaterOrEqual(t, strings.Count(batch, field), 3)
				require.GreaterOrEqual(t, strings.Count(bestEffort, field), 2)
			}
		})
	}
}
