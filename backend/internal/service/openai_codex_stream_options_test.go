package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 固定 Codex 0.153.3 的显式字段契约；兼容参数不得导致原生摘要交付选项被整包删除。
func TestCodexStreamOptionsNormalization(t *testing.T) {
	cases := []struct {
		name    string
		options string
		keep    bool
	}{
		{name: "absent"},
		{name: "native", options: `{"reasoning_summary_delivery":"sequential_cutoff"}`, keep: true},
		{name: "native_with_whitespace", options: `{ "reasoning_summary_delivery" : "sequential_cutoff" }`, keep: true},
		{name: "mixed_chat_option", options: `{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}`, keep: true},
		{name: "mixed_unknown", options: `{"reasoning_summary_delivery":"sequential_cutoff","unknown":{"nested":true}}`, keep: true},
		{name: "chat_only", options: `{"include_usage":true}`},
		{name: "unknown_value", options: `{"reasoning_summary_delivery":"future_value"}`},
		{name: "empty_string", options: `{"reasoning_summary_delivery":""}`},
		{name: "non_string", options: `{"reasoning_summary_delivery":true}`},
		{name: "empty_object", options: `{}`},
		{name: "null", options: `null`},
		{name: "boolean", options: `true`},
		{name: "array", options: `[{"reasoning_summary_delivery":"sequential_cutoff"}]`},
	}
	for _, tc := range cases {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compact=%v", tc.name, compact), func(t *testing.T) {
				body := []byte(`{"model":"gpt-6-astra","instructions":"synthetic","stream":true,"store":false,"input":[]}`)
				if tc.options != "" {
					body = append(body[:len(body)-1], []byte(`,"stream_options":`+tc.options+`}`)...)
				}
				before := bytes.Clone(body)
				var parsed map[string]any
				require.NoError(t, json.Unmarshal(body, &parsed))
				applyCodexOAuthTransform(parsed, true, compact)
				normalized, _, err := normalizeOpenAIPassthroughOAuthBody(body, compact)
				require.NoError(t, err)
				require.Equal(t, before, body, "原始透传缓冲区不得被改写")
				want := tc.keep && !compact
				require.Equal(t, want, gjson.GetBytes(normalized, "stream_options").Exists())
				_, exists := parsed["stream_options"]
				require.Equal(t, want, exists)
				if want {
					expected := `{"reasoning_summary_delivery":"sequential_cutoff"}`
					value, err := json.Marshal(parsed["stream_options"])
					require.NoError(t, err)
					require.JSONEq(t, expected, string(value))
					require.JSONEq(t, expected, gjson.GetBytes(normalized, "stream_options").Raw)
					if tc.name == "native_with_whitespace" {
						require.Equal(t, tc.options, gjson.GetBytes(normalized, "stream_options").Raw)
					}
				}
			})
		}
	}
}

var codexStreamOptionsTestAccountID atomic.Int64

// 从真实 Forward 验证两种模型、普通/透传与缺省/显式请求，而非只测过滤辅助函数。
func TestCodexStreamOptionsForward(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol"} {
		for _, passthrough := range []bool{false, true} {
			for _, supplied := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/passthrough=%v/supplied=%v", model, passthrough, supplied), func(t *testing.T) {
					body := map[string]any{
						"model": model, "instructions": "synthetic", "stream": true,
						"input": []any{}, "prompt_cache_key": " explicit key ",
						"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"},
						"client_metadata": map[string]any{
							"session_id": "synthetic-session", "thread_id": "synthetic-thread",
							"turn_id": "01993000-0000-7000-8000-000000000510",
						},
					}
					if supplied {
						body["stream_options"] = map[string]any{"reasoning_summary_delivery": "sequential_cutoff"}
					}
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					before := bytes.Clone(raw)
					account := newTestOAuthAccount(996000+codexStreamOptionsTestAccountID.Add(1), map[string]any{
						codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
					})
					account.Credentials = map[string]any{"access_token": "synthetic-token"}
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
						Body: io.NopCloser(strings.NewReader(
							"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
								"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n")),
					}}
					svc := &OpenAIGatewayService{
						accountRepo: &codexIdentityPersistenceRepo{account: account}, httpUpstream: upstream,
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
					c.Request.Header.Set("User-Agent", "codex-tui/0.153.3 (Windows 10.0.26200; x86_64) xterm-256color")
					c.Request.Header.Set("session-id", "synthetic-session")
					c.Request.Header.Set("thread-id", "synthetic-thread")
					SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
					_, err = svc.Forward(context.Background(), c, account, raw)
					require.NoError(t, err)
					require.Equal(t, before, raw)
					require.Len(t, upstream.bodies, 1)
					out := upstream.lastBody
					require.Equal(t, supplied, gjson.GetBytes(out, "stream_options").Exists())
					if supplied {
						require.Equal(t, "sequential_cutoff", gjson.GetBytes(out, "stream_options.reasoning_summary_delivery").String())
					}
					require.Equal(t, model, gjson.GetBytes(out, "model").String())
					require.Equal(t, "xhigh", gjson.GetBytes(out, "reasoning.effort").String())
					require.Equal(t, "auto", gjson.GetBytes(out, "reasoning.summary").String())
					require.Equal(t, " explicit key ", gjson.GetBytes(out, "prompt_cache_key").String())
				})
			}
		}
	}
}
