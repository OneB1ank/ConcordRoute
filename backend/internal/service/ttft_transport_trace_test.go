//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 连接回调、请求级操作和旧尝试的迟到事件分别保留；原始头、正文及 context 不受影响。
func TestTTFTTransportTraceAttemptIsolationAndRequestParity(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	InitTTFTStageTiming(c, true)
	ctx := c.Request.Context()
	latencytrace.Start(ctx, "account_selection")(nil)
	var lastTrace *httptrace.ClientTrace
	for _, accountID := range []int64{11, 22} {
		req, err := http.NewRequestWithContext(ctx, "POST", "https://example.invalid", strings.NewReader(`{"prompt_cache_key":" original ","input":"test"}`))
		require.NoError(t, err)
		req.Header.Set("User-Agent", "unchanged-UA")
		req.Header.Set("Originator", "unchanged-originator")
		req.Header.Set("Version", "unchanged-version")
		req.Header.Set("Session_id", "unchanged-session")
		headers := req.Header.Clone()
		mark := BeginTTFTUpstreamAttempt(c, accountID)
		req = withTTFTUpstreamTrace(c, req, mark)
		trace := httptrace.ContextClientTrace(req.Context())
		require.NotNil(t, trace)
		trace.GetConn("SECRET_HOST")
		if lastTrace != nil {
			lastTrace.WroteRequest(httptrace.WroteRequestInfo{Err: context.Canceled})
		}
		trace.GotConn(httptrace.GotConnInfo{Reused: accountID == 22})
		trace.GotFirstResponseByte()
		lastTrace = trace
		require.Equal(t, headers, req.Header)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.Equal(t, `{"prompt_cache_key":" original ","input":"test"}`, string(body))
	}
	attempts := TTFTAttemptTimingSnapshots(c)
	require.Len(t, attempts, 2)
	require.Len(t, attempts[0].Transport.Events, 4)
	require.Len(t, attempts[1].Transport.Events, 3)
	require.Equal(t, "request_written", attempts[0].Transport.Events[3].Phase)
	require.True(t, attempts[0].Transport.Events[3].Failed)
	data, err := json.Marshal(attempts)
	require.NoError(t, err)
	require.NotContains(t, string(data), "SECRET")
	require.Len(t, TTFTRequestOperationSnapshot(c).Events, 2)
	*attempts[0].Transport.Events[1].Reused = true
	require.False(t, *TTFTAttemptTimingSnapshots(c)[0].Transport.Events[1].Reused)
}

// 通过真实 token provider 的热命中和锁竞争取消验证接线，不改 token 或刷新策略。
func TestTTFTTokenProviderCacheAndLockWait(t *testing.T) {
	cache := newOpenAITokenCacheStub()
	account := &Account{ID: 82, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	cache.tokens[OpenAITokenCacheKey(account)] = "SENSITIVE_TOKEN"
	provider := NewOpenAITokenProvider(nil, cache, nil)
	svc := &OpenAIGatewayService{openAITokenProvider: provider}
	recorder := latencytrace.New(time.Now())
	ctx := latencytrace.WithRecorder(context.Background(), recorder)
	token, _, err := svc.GetAccessToken(ctx, account)
	require.NoError(t, err)
	require.Equal(t, "SENSITIVE_TOKEN", token)
	raw, err := json.Marshal(recorder.Snapshot())
	require.NoError(t, err)
	require.Contains(t, string(raw), "token_get_done")
	require.Contains(t, string(raw), "token_cache_hit")
	require.NotContains(t, string(raw), "SENSITIVE")
	ctx, cancel := context.WithTimeout(ctx, 35*time.Millisecond)
	defer cancel()
	_, err = provider.waitForTokenAfterLockRace(ctx, "empty-key")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	events := recorder.Snapshot().Events
	require.Equal(t, "token_lock_wait_done", events[len(events)-1].Phase)
	require.True(t, events[len(events)-1].Failed)
}

func TestTTFTTransportTraceDisabledKeepsRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req := httptest.NewRequest("POST", "/", nil)
	require.Same(t, req, withTTFTUpstreamTrace(c, req, noopTTFTStage))
	require.Nil(t, TTFTRequestOperationSnapshot(c))
}
