//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIOAuthCompactConvergesFingerprintHeadersWithoutChangingSchema(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "transform"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			account := newTestOAuthAccount(1801, map[string]any{
				codexFingerprintModeExtraKey: codexFingerprintCockpit,
				"openai_passthrough":         passthrough,
			})
			account.Concurrency = 1
			account.Credentials = make(map[string]any)
			account.Credentials["access_token"] = "test-token"
			account.Credentials["chatgpt_account_id"] = "test-account"

			body := []byte(`{"model":"gpt-5.4","stream":true,"store":true,"prompt_cache_key":"client-cache","input":"hello","client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-installation-id":"client-installation"}}`)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"stop after capture"}}`)),
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}

			_, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, resolveConvergedInstallationID(account), upstream.lastReq.Header.Get("x-codex-installation-id"))
			require.Equal(t, resolveConvergedSessionID(account), upstream.lastReq.Header.Get("session-id"))
			require.Equal(t, resolveConvergedThreadID(account, "client-thread"), upstream.lastReq.Header.Get("thread-id"))
			require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
		})
	}
}
