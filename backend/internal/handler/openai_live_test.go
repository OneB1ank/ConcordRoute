package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestParseLiveCallRequestMultipartPreservesSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	session := `{"model":"gpt-live-test","delegation":{"type":"client"},"instructions":"你好"}`
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.WriteField("sdp", "v=0\r\n"))
	require.NoError(t, writer.WriteField("session", session))
	require.NoError(t, writer.Close())

	request := httptest.NewRequest("POST", "/v1/live", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request

	parsed, err := parseLiveCallRequest(context)
	require.NoError(t, err)
	require.Equal(t, "v=0\r\n", parsed.SDP)
	require.JSONEq(t, session, string(parsed.Session))
	require.Equal(t, "client", jsonPathString(t, parsed.Session, "delegation", "type"))
}

func TestParseLiveCallRequestJSONPreservesSessionWithoutDelegation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := `{"sdp":"v=0\\r\\n","session":{"model":"gpt-live-test","instructions":"standalone"}}`
	request := httptest.NewRequest("POST", "/backend-api/codex/realtime/calls", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request

	parsed, err := parseLiveCallRequest(context)
	require.NoError(t, err)
	require.NotContains(t, string(parsed.Session), "delegation")
	require.Equal(t, "standalone", jsonPathString(t, parsed.Session, "instructions"))
}

func TestParseLiveCallRequestRejectsInvalidJSONShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	testCases := []string{
		`{"session":{"type":"quicksilver"}}`,
		`{"sdp":"v=0\\r\\n","session":[]}`,
		`{"sdp":"v=0\\r\\n","session":null}`,
		`{"sdp":"v=0\\r\\n","session":{"type":"quicksilver"}} {}`,
	}
	for _, body := range testCases {
		request := httptest.NewRequest("POST", "/backend-api/codex/realtime/calls", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = request
		_, err := parseLiveCallRequest(context)
		require.Error(t, err)
	}
}

func TestLiveSidebandLocationMatchesCreateRoute(t *testing.T) {
	require.Equal(t, "/v1/live/call_123", liveSidebandLocation("/v1/live", "call_123"))
	require.Equal(
		t,
		"/backend-api/codex/call_123",
		liveSidebandLocation("/backend-api/codex/realtime/calls", "call_123"),
	)
}

// Pi 等客户端按原生201判断创建成功；200会导致有效SDP被丢弃并留下空闲会话。
func TestLiveCreatedResponseMatchesNativeContract(t *testing.T) {
	for _, route := range []string{"/v1/live", "/backend-api/codex/realtime/calls"} {
		t.Run(route, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.POST(route, func(c *gin.Context) {
				writeLiveCallCreated(c, &service.LiveCallCreated{
					CallID: "call_test", SDP: []byte("v=answer\r\n"),
				})
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, route, nil))
			require.Equal(t, http.StatusCreated, response.Code)
			require.Equal(t, "application/sdp", response.Header().Get("Content-Type"))
			require.Equal(t, liveSidebandLocation(route, "call_test"), response.Header().Get("Location"))
			require.Equal(t, "v=answer\r\n", response.Body.String())
		})
	}
}

func TestLiveEnabledForAPIKey(t *testing.T) {
	require.False(t, liveEnabledForAPIKey(nil))
	require.False(t, liveEnabledForAPIKey(&service.APIKey{}))
	require.False(t, liveEnabledForAPIKey(&service.APIKey{
		Group: &service.Group{Platform: service.PlatformOpenAI},
	}))
	require.False(t, liveEnabledForAPIKey(&service.APIKey{
		Group: &service.Group{Platform: service.PlatformAnthropic, AllowLive: true},
	}))
	require.True(t, liveEnabledForAPIKey(&service.APIKey{
		Group: &service.Group{Platform: service.PlatformOpenAI, AllowLive: true},
	}))
}

func TestLiveCallIdentityPreservesClientSessionHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodPost, "/backend-api/codex/realtime/calls", nil)
	context.Request.Header.Set("session-id", "root-session")
	context.Request.Header.Set("thread-id", "child-thread")
	identity := liveCallIdentity(context, &service.APIKey{ID: 1, UserID: 2}, 2, nil)
	// 原生 Live 在 Header 发送根会话/线程，Body 的 session_id 是另一层语音标识。
	require.Equal(t, "root-session", identity.ClientSessionID)
	require.Equal(t, "child-thread", identity.ClientThreadID)
}

func TestLiveContentModerationBlocksBeforeBilling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	moderationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/moderations", r.URL.Path)
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"sexual":0.9}}]}`))
	}))
	defer moderationServer.Close()

	cfg := &service.ContentModerationConfig{
		Enabled:      true,
		Mode:         service.ContentModerationModePreBlock,
		BaseURL:      moderationServer.URL,
		Model:        "omni-moderation-latest",
		APIKeys:      []string{"sk-test"},
		SampleRate:   100,
		AllGroups:    true,
		BlockMessage: "Live 内容审核测试阻断",
	}
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)
	moderationSvc := service.NewContentModerationService(
		&contentModerationHandlerSettingRepo{values: map[string]string{
			service.SettingKeyRiskControlEnabled:      "true",
			service.SettingKeyContentModerationConfig: string(rawCfg),
		}},
		&contentModerationHandlerTestRepo{},
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	groupID := int64(2)
	group := &service.Group{ID: groupID, Name: "openai", Platform: service.PlatformOpenAI, AllowLive: true}
	apiKey := &service.APIKey{
		ID:      101,
		Name:    "live-test-key",
		GroupID: &groupID,
		Group:   group,
		UserID:  1,
		User:    &service.User{ID: 1},
	}
	body := bytes.NewBufferString(`{"sdp":"v=0\\r\\n","session":{"model":"gpt-live-test","input":"bad prompt"}}`)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodPost, "/v1/live", body)
	context.Request.Header.Set("Content-Type", "application/json")
	context.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	context.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 1, Concurrency: 1})

	// 不注入计费服务，以状态码证明内容审核在计费检查之前完成阻断。
	(&OpenAIGatewayHandler{contentModerationService: moderationSvc}).Live(context)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "content_policy_violation")
	require.Contains(t, recorder.Body.String(), "Live 内容审核测试阻断")
	requestType, exists := context.Get(opsRequestTypeKey)
	require.True(t, exists)
	require.Equal(t, int16(service.RequestTypeLive), requestType)
}

func TestLiveAttestationErrorIsExplicit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)

	(&OpenAIGatewayHandler{}).writeLiveCreateError(context, &service.LiveAttestationUnavailableError{
		Reason: "Live attestation is only supported when ConcordRoute runs on macOS",
	})

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "ConcordRoute runs on macOS")
}

func TestLiveNoEligibleAccountIsNotUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	// 调度尚未发送上游请求时，不能把本地缺少候选账号误报成上游 502。
	(&OpenAIGatewayHandler{}).writeLiveCreateError(context,
		fmt.Errorf("model unavailable: %w", service.ErrNoAvailableAccounts))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Contains(t, recorder.Body.String(), "No eligible accounts")
	require.NotContains(t, recorder.Body.String(), "upstream request failed")
}

func jsonPathString(t *testing.T, raw json.RawMessage, keys ...string) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal(raw, &value))
	current := value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		require.True(t, ok)
		current = object[key]
	}
	result, ok := current.(string)
	require.True(t, ok)
	return result
}
