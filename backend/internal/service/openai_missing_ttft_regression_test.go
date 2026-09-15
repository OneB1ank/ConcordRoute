package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 终态才返回的可见内容也应有首字；模拟真实转发入口，区分有内容和只有用量的流。
func TestOpenAIResponsesTTFTMissingTerminalContent(t *testing.T) {
	const refusal = `{"type":"message","id":"msg_test","role":"assistant","content":[{"type":"refusal","refusal":"synthetic refusal"}]}`
	for _, tc := range []struct {
		name, event, terminal string
		want                  bool
	}{
		{"refusal_done", `{"type":"response.refusal.done","refusal":"synthetic refusal"}`, `{"type":"response.completed","response":{"id":"resp_test","output":[],"usage":{"input_tokens":12,"output_tokens":3}}}`, true},
		{"refusal_part", `{"type":"response.content_part.done","part":{"type":"refusal","refusal":"synthetic refusal"}}`, `{"type":"response.completed","response":{"id":"resp_test","output":[],"usage":{"input_tokens":12,"output_tokens":3}}}`, true},
		{"refusal_item", `{"type":"response.output_item.done","item":` + refusal + `}`, `{"type":"response.completed","response":{"id":"resp_test","output":[],"usage":{"input_tokens":12,"output_tokens":3}}}`, true},
		{"refusal_terminal", "", `{"type":"response.completed","response":{"id":"resp_test","output":[` + refusal + `],"usage":{"input_tokens":12,"output_tokens":3}}}`, true},
		{"incomplete_text", "", `{"type":"response.incomplete","response":{"id":"resp_test","status":"incomplete","output":[{"type":"message","content":[{"type":"output_text","text":"partial output"}]}],"usage":{"input_tokens":12,"output_tokens":3}}}`, true},
		{"incomplete_usage_only", "", `{"type":"response.incomplete","response":{"id":"resp_test","status":"incomplete","output":[],"usage":{"input_tokens":12,"output_tokens":3}}}`, false},
	} {
		for _, route := range []string{"native", "guarded", "passthrough"} {
			for _, ending := range []string{"blank", "eof"} {
				t.Run(tc.name+"/"+route+"/"+ending, func(t *testing.T) {
					body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n"
					if tc.event != "" {
						body += "data: " + tc.event + "\n\n"
					}
					body += "data: " + tc.terminal
					if ending == "blank" {
						body += "\n\n"
					}
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					if route == "guarded" {
						svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
					account := &Account{ID: 1, Platform: PlatformOpenAI}
					started := time.Now().Add(-80 * time.Millisecond)
					var first *int
					var usage *OpenAIUsage
					if route == "passthrough" {
						result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, started, "test-model", "test-model")
						require.NoError(t, err)
						first, usage = result.firstTokenMs, result.usage
					} else {
						result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test-model", "test-model")
						require.NoError(t, err)
						first, usage = result.firstTokenMs, result.usage
					}
					require.Equal(t, 12, usage.InputTokens)
					require.Equal(t, 3, usage.OutputTokens)
					if tc.want {
						require.NotNil(t, first, "真实内容已发送，不应仍记录空首字")
						require.GreaterOrEqual(t, *first, 80)
						require.LessOrEqual(t, *first, int(time.Since(started).Milliseconds()))
						require.Contains(t, rec.Body.String(), map[bool]string{true: "partial output", false: "synthetic refusal"}[tc.name == "incomplete_text"])
					} else {
						require.Nil(t, first, "仅有用量不代表存在首内容")
					}
				})
			}
		}
	}
}
