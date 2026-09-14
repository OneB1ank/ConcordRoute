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

// 真实客户端的工具发现调用使用对象参数；使用合成内容复现“有输出但首字为空”。
func TestOpenAIResponsesTTFTToolSearchObjectArguments(t *testing.T) {
	const item = `{"type":"tool_search_call","id":"tool_search_test","call_id":"call_test","status":"completed","execution":"client","arguments":{"query":"synthetic tool","limit":1}}`
	for _, route := range []string{"native", "native_guarded", "passthrough"} {
		for _, ending := range []string{"blank_line", "eof"} {
			for _, mode := range []string{"item_done", "terminal_only"} {
				t.Run(route+"/"+ending+"/"+mode, func(t *testing.T) {
					gin.SetMode(gin.TestMode)
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					if route == "native_guarded" {
						svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
					}
					body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n"
					if mode == "item_done" {
						body += "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":" + item + "}\n\n"
					}
					body += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[" + item + "],\"usage\":{\"input_tokens\":350,\"output_tokens\":28}}}"
					if ending == "blank_line" {
						body += "\n\n"
					}
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					InitTTFTStageTiming(c, true)
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
					account := &Account{ID: 1, Platform: PlatformOpenAI}
					started := time.Now().Add(-80 * time.Millisecond)
					var first *int
					var usage *OpenAIUsage
					if route == "passthrough" {
						result, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, started, "test-model", "test-model")
						require.NoError(t, err)
						require.NotNil(t, result)
						first, usage = result.firstTokenMs, result.usage
					} else {
						result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test-model", "test-model")
						require.NoError(t, err)
						require.NotNil(t, result)
						first, usage = result.firstTokenMs, result.usage
					}
					require.Contains(t, rec.Body.String(), item, "工具参数必须保持对象，统计修复不得改写正文")
					require.Equal(t, 350, usage.InputTokens)
					require.Equal(t, 28, usage.OutputTokens)
					require.NotNil(t, first, "工具发现调用已写出，首字不应为空")
					require.GreaterOrEqual(t, *first, 80)
					require.LessOrEqual(t, *first, int(time.Since(started).Milliseconds()))
					require.Contains(t, TTFTStageTimingSnapshot(c, time.Now()), "first_content_received")
				})
			}
		}
	}
}

// 先到达的空工具项仍只是结构，已填充的参数在完成事件之前到达时就应记录首内容。
func TestOpenAIResponsesTTFTToolSearchWaitsForArguments(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, timeout := range []int{0, 1} {
			result := runSyntheticVisibleTTFTStream(t, passthrough, 80*time.Millisecond, timeout,
				`{"type":"response.output_item.done","item":{"type":"tool_search_call","status":"completed","execution":"client","arguments":{"query":"synthetic tool","limit":1}}}`)
			require.NotNil(t, result.firstTokenMs)
			require.GreaterOrEqual(t, *result.firstTokenMs, 70)
		}
	}
}
