//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// PostgreSQL 回环覆盖两条写入入口及 NULL/非标准 HTTP 状态；依赖既有迁移测试容器。
func TestUsageLogObservationPersistence(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := newUsageLogRepositoryWithSQL(client, integrationDB)
	user := mustCreateUser(t, client, &service.User{Email: "obs-" + uuid.NewString() + "@example.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-test-" + uuid.NewString(), Name: "test"})
	account := mustCreateAccount(t, client, &service.Account{Name: "obs-" + uuid.NewString()})
	for _, bestEffort := range []bool{false, true} {
		for _, code := range []int{0, 200, 201, 202, 299} {
			log := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: account.ID,
				RequestID: uuid.NewString(), Model: "test", InputTokens: 8, OutputTokens: 2,
				TotalCost: 0.01, ActualCost: 0.01, CreatedAt: time.Now().UTC()}
			size := 0
			if code == 200 {
				size = 356
			}
			if code != 0 {
				log.UpstreamStatusCode, log.CodexTurnStateBytes = &code, &size
			}
			if bestEffort {
				require.NoError(t, repo.CreateBestEffort(ctx, log))
				// best-effort 不承诺回填 ID，按真实幂等键读取落库结果。
				require.NoError(t, integrationDB.QueryRowContext(ctx,
					"SELECT id FROM usage_logs WHERE request_id = $1 AND api_key_id = $2", log.RequestID, key.ID).Scan(&log.ID))
			} else {
				_, err := repo.Create(ctx, log)
				require.NoError(t, err)
			}
			got, err := repo.GetByID(ctx, log.ID)
			require.NoError(t, err)
			require.Equal(t, log.UpstreamStatusCode, got.UpstreamStatusCode)
			require.Equal(t, log.CodexTurnStateBytes, got.CodexTurnStateBytes)
			require.Equal(t, log.ActualCost, got.ActualCost)
		}
	}
}
