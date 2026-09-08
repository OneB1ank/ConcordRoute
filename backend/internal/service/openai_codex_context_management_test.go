package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 依据 Codex rust-v0.153.4 的工具与上下文格式构造合成请求。
// 本夹具只验证 Responses 承载，不表示原生 history/notes 后端已实现。
func codexExperimentalContextFixture(t *testing.T) []byte {
	t.Helper()
	emptyObject := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
	function := func(name string) map[string]any {
		return map[string]any{"type": "function", "name": name, "description": "synthetic context tool", "strict": false, "parameters": emptyObject}
	}
	body, err := json.Marshal(map[string]any{
		"model": "gpt-6-astra", "stream": true,
		"instructions": "synthetic context test", "prompt_cache_key": "context-feature-cache",
		"tools": []any{
			function("new_context"), function("get_context_remaining"),
			map[string]any{"type": "namespace", "name": "history", "description": "synthetic history tools", "tools": []any{function("read_item")}},
			map[string]any{"type": "namespace", "name": "notes", "description": "synthetic note tools", "tools": []any{function("write_file")}},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{
				"type": "input_text",
				"text": "<context_window>\nAgent name: /root\nFirst context window id: 01990000-0000-7000-8000-000000000001\nCurrent context window id: 01990000-0000-7000-8000-000000000002\nPrevious context window id: 01990000-0000-7000-8000-000000000001\n</context_window>",
			}}},
			// 使用已规范化的调用标识，旧 call_* → fc_* 的配对兼容另有测试覆盖。
			map[string]any{"type": "function_call", "namespace": "history", "name": "read_item", "call_id": "fc_context_history", "arguments": `{"item_id":"synthetic-item"}`},
			map[string]any{"type": "function_call_output", "call_id": "fc_context_history", "output": []any{
				map[string]any{"type": "encrypted_content", "encrypted_content": "synthetic-encrypted-history-output"},
			}},
		},
		"client_metadata": map[string]any{"session_id": "context-feature-session", "thread_id": "context-feature-thread", "window_number": "1"},
	})
	require.NoError(t, err)
	return body
}

// 输入上下文、命名空间和不透明工具结果属于客户端协议，转发时不得丢失。
func requireCodexExperimentalContextWire(t *testing.T, original, forwarded []byte) {
	t.Helper()
	require.JSONEq(t, gjson.GetBytes(original, "tools").Raw, gjson.GetBytes(forwarded, "tools").Raw)
	require.JSONEq(t, gjson.GetBytes(original, "input").Raw, gjson.GetBytes(forwarded, "input").Raw)
	require.Equal(t, "context-feature-cache", gjson.GetBytes(forwarded, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(forwarded, "context_management").Exists(), "客户端配置不是新增的 Responses 顶层参数")
	require.False(t, gjson.GetBytes(forwarded, "client_metadata.root_turn_id").Exists())
}

// 普通 OAuth 转换和原生透传均通过真实 Forward 入口验证，不连接外部模型。
func TestCodexExperimentalContextResponsesHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "transform"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			account := newTestOAuthAccount(18401, map[string]any{
				codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
			})
			account.Credentials = map[string]any{"access_token": "synthetic-context-token"}
			account.Concurrency = 1
			body := codexExperimentalContextFixture(t)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic capture stop"}}`)),
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			_, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err, "在出站捕获后主动终止测试")
			require.NotNil(t, upstream.lastReq)
			requireCodexExperimentalContextWire(t, body, upstream.lastBody)
		})
	}
}

// 两种 WS 入口同样验证工具承载；这不是 history/notes 独立接口的端到端测试。
func TestCodexExperimentalContextResponsesWS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
			cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
			upstream := &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.completed","response":{"id":"resp_context_test","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`),
			}}
			dialer := &openAIWSCaptureDialer{conn: upstream}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{
				cfg: cfg, cache: &stubGatewayCache{}, httpUpstream: &httpUpstreamRecorder{},
				openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(),
				openaiWSPool: pool, openaiWSPassthroughDialer: dialer,
			}
			account := newTestOAuthAccount(18402, map[string]any{
				codexFingerprintModeExtraKey: "cockpit", "openai_oauth_responses_websockets_v2_mode": mode,
			})
			account.Credentials = map[string]any{"access_token": "synthetic-context-token"}
			account.Concurrency = 1
			serverResult := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := coderws.Accept(w, r, nil)
				if err != nil {
					serverResult <- err
					return
				}
				defer func() { _ = conn.CloseNow() }()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = r.Clone(r.Context())
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				_, body, err := conn.Read(ctx)
				if err == nil {
					err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-context-token", body, nil)
				}
				serverResult <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			body := codexExperimentalContextFixture(t)
			body, err = sjson.SetBytes(body, "type", "response.create")
			require.NoError(t, err)
			require.NoError(t, client.Write(ctx, coderws.MessageText, body))
			_, event, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "resp_context_test", gjson.GetBytes(event, "response.id").String())
			_ = client.Close(coderws.StatusNormalClosure, "done")
			select {
			case err := <-serverResult:
				if err != nil {
					require.True(t, isOpenAIWSClientDisconnectError(err), "%v", err)
				}
			case <-ctx.Done():
				t.Fatal("等待本地 WS 转发结束超时")
			}
			require.Len(t, upstream.writes, 1)
			requireCodexExperimentalContextWire(t, body, []byte(requestToJSONString(upstream.writes[0])))
		})
	}
}
