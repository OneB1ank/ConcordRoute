package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIHTTPBindingReadCloser struct {
	data []byte
	err  error
}

func (r *openAIHTTPBindingReadCloser) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.err != nil {
		err := r.err
		r.err = nil
		return 0, err
	}
	return 0, io.EOF
}

func (r *openAIHTTPBindingReadCloser) Close() error { return nil }

func TestOpenAIHTTPResponseBinding_OnlySuccessfulTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	paths := []struct {
		name        string
		passthrough bool
	}{
		{name: "普通转发"},
		{name: "透传转发", passthrough: true},
	}
	streamCases := []struct {
		name         string
		terminal     string
		wantBind     bool
		wantErr      bool
		transportErr bool
	}{
		{name: "completed", terminal: "response.completed", wantBind: true},
		{name: "done", terminal: "response.done", wantBind: true},
		{name: "failed", terminal: "response.failed", wantErr: true},
		{name: "incomplete", terminal: "response.incomplete"},
		{name: "cancelled", terminal: "response.cancelled"},
		{name: "仅 DONE", terminal: "[DONE]"},
		{name: "成功终态后传输错误", terminal: "response.completed", transportErr: true},
	}
	nonStreamCases := []struct {
		name     string
		status   string
		typeName string
		wantBind bool
	}{
		{name: "completed", status: "completed", wantBind: true},
		{name: "done", status: "done", typeName: "response.done", wantBind: true},
		{name: "failed", status: "failed"},
		{name: "incomplete", status: "incomplete"},
		{name: "cancelled", status: "cancelled"},
		{name: "失败事件优先于状态字段", status: "completed", typeName: "response.incomplete"},
		{name: "失败状态优先于事件字段", status: "incomplete", typeName: "response.completed"},
		{name: "缺少终态", status: ""},
	}

	for _, path := range paths {
		path := path
		t.Run(path.name, func(t *testing.T) {
			for caseIndex, tc := range streamCases {
				tc := tc
				t.Run("流式/"+tc.name, func(t *testing.T) {
					responseID := fmt.Sprintf("resp_http_stream_%t_%d", path.passthrough, caseIndex)
					body := openAIHTTPBindingSSE(responseID, tc.terminal)
					responseBody := io.NopCloser(strings.NewReader(body))
					if tc.transportErr {
						responseBody = &openAIHTTPBindingReadCloser{data: []byte(body), err: errors.New("upstream read failed")}
					}
					testOpenAIHTTPResponseBinding(t, path.passthrough, true, responseID, responseBody, tc.wantBind, tc.wantErr)
				})
			}

			for caseIndex, tc := range nonStreamCases {
				tc := tc
				t.Run("非流式/"+tc.name, func(t *testing.T) {
					responseID := fmt.Sprintf("resp_http_json_%t_%d", path.passthrough, caseIndex)
					body := fmt.Sprintf(`{"id":%q,"type":%q,"status":%q,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, responseID, tc.typeName, tc.status)
					testOpenAIHTTPResponseBinding(t, path.passthrough, false, responseID, io.NopCloser(strings.NewReader(body)), tc.wantBind, false)
				})
			}
		})
	}
}

func testOpenAIHTTPResponseBinding(
	t *testing.T,
	passthrough bool,
	stream bool,
	responseID string,
	responseBody io.ReadCloser,
	wantBind bool,
	wantErr bool,
) {
	t.Helper()
	groupID := int64(49401)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestPath := "/openai/v1/responses"
	if passthrough && !stream {
		requestPath = "/openai/v1/responses/compact"
	}
	c.Request = httptest.NewRequest(http.MethodPost, requestPath, nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.145.0")
	c.Set("api_key", &APIKey{ID: 49402, UserID: 49403, GroupID: &groupID})
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{map[bool]string{true: "text/event-stream", false: "application/json"}[stream]},
			"x-request-id": []string{"rid_http_binding"},
		},
		Body: responseBody,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := openAIHTTPBindingAccount(passthrough)
	requestBody := []byte(fmt.Sprintf(`{"model":"gpt-5.4","stream":%t,"store":true,"instructions":"test","input":"hello"}`, stream))

	result, err := svc.Forward(context.Background(), c, account, requestBody)
	if wantErr {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
		require.NotNil(t, result)
	}

	boundAccountID, getErr := svc.getOpenAIWSStateStore().GetResponseAccount(context.Background(), groupID, responseID)
	require.NoError(t, getErr)
	if wantBind {
		require.Equal(t, account.ID, boundAccountID)
	} else {
		require.Zero(t, boundAccountID)
	}
}

func openAIHTTPBindingAccount(passthrough bool) *Account {
	if passthrough {
		return &Account{
			ID: 49411, Name: "oauth-passthrough", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
			Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"},
			Extra: map[string]any{
				"openai_passthrough":                        true,
				"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff,
			},
			Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
		}
	}
	return &Account{
		ID: 49410, Name: "api-key", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://example.com"},
		Extra:       map[string]any{"use_responses_api": true},
		Status:      StatusActive, Schedulable: true, RateMultiplier: f64p(1),
	}
}

func openAIHTTPBindingSSE(responseID, terminal string) string {
	if terminal == "[DONE]" {
		return fmt.Sprintf("data: {\"type\":\"response.created\",\"response\":{\"id\":%q}}\n\ndata: [DONE]\n\n", responseID)
	}
	status := strings.TrimPrefix(terminal, "response.")
	terminalPayload := fmt.Sprintf(`{"type":%q,"response":{"id":%q,"status":%q,"output":[{"type":"message","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`, terminal, responseID, status)
	if terminal == "response.failed" {
		terminalPayload = fmt.Sprintf(`{"type":"response.failed","response":{"id":%q,"status":"failed","error":{"message":"upstream failed"},"usage":{"input_tokens":1,"output_tokens":0}}}`, responseID)
	}
	return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" + "data: " + terminalPayload + "\n\n"
}
