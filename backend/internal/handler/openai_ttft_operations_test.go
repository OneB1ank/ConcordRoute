package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// 真实账号槽位等待入口把超时记录为失败，并与已有已获槽位路径区分；采样不改变响应。
func TestOpenAITTFTAccountSlotWaitAndImmediate(t *testing.T) {
	for _, acquired := range []bool{false, true} {
		var baselineBody string
		var baselineCode int
		for _, enabled := range []bool{false, true} {
			c, response := newHelperTestContext(http.MethodPost, "/v1/responses")
			service.InitTTFTStageTiming(c, enabled)
			cache := &helperConcurrencyCacheStub{accountSeq: []bool{false}}
			h := &OpenAIGatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)}
			selection := &service.AccountSelectionResult{
				Account:  &service.Account{ID: 51, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
				Acquired: acquired, ReleaseFunc: func() {},
				WaitPlan: &service.AccountWaitPlan{MaxConcurrency: 1, MaxWaiting: 1, Timeout: 60 * time.Millisecond},
			}
			streamStarted := false
			release, ok := h.acquireResponsesAccountSlot(c, nil, "", selection, false, &streamStarted, zap.NewNop())
			require.Equal(t, acquired, ok)
			if release != nil {
				release()
			}
			if !enabled {
				require.Nil(t, service.TTFTRequestOperationSnapshot(c))
				baselineBody, baselineCode = response.Body.String(), response.Code
				continue
			}
			require.Equal(t, baselineBody, response.Body.String())
			require.Equal(t, baselineCode, response.Code)
			snapshot := service.TTFTRequestOperationSnapshot(c)
			require.Len(t, snapshot.Events, 2)
			require.Equal(t, !acquired, snapshot.Events[1].Failed)
			if !acquired {
				require.GreaterOrEqual(t, snapshot.Events[1].AtMS-snapshot.Events[0].AtMS, int64(50))
			}
			raw, err := json.Marshal(snapshot)
			require.NoError(t, err)
			t.Logf("acquired=%v %s", acquired, raw)
		}
	}
}
