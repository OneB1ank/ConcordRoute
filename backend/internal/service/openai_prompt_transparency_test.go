package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 仅供回归断言使用，生产代码不再构造这两类提示。
const (
	codexImageGenerationBridgeMarker = "<sub2api-codex-image-generation>"
	codexSparkImageUnsupportedMarker = "<sub2api-codex-spark-image-unsupported>"
)

// 同时覆盖两种凭据和有无任务工具，避免只删除 OAuth 分支或改成按工具注入。
func TestPromptTransparencyMessagesPreservesClientMessagesAndTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, model := range []string{"gpt-5.5", "gpt-6-astra"} {
			for _, withTools := range []bool{false, true} {
				name := accountType + "/" + model + "/without_tools"
				if withTools {
					name = accountType + "/" + model + "/with_tools"
				}
				t.Run(name, func(t *testing.T) {
					upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_transparency", model)}
					svc := &OpenAIGatewayService{
						httpUpstream: upstream,
						cfg:          &config.Config{},
					}
					account := &Account{
						ID: 1, Platform: PlatformOpenAI, Type: accountType, Concurrency: 1,
						Credentials: map[string]any{"access_token": "test-token", "api_key": "test-key"},
						Extra:       map[string]any{"openai_responses_mode": "responses"},
					}
					request := map[string]any{
						"model": "claude-sonnet-4-5", "max_tokens": 16,
						"system": "project instructions", "stream": false,
						"messages": []any{map[string]any{"role": "user", "content": "review files"}},
					}
					if withTools {
						request["tools"] = []any{map[string]any{
							"name": "TodoWrite", "description": "client task tool",
							"input_schema": map[string]any{"type": "object"},
						}}
					}
					body, err := json.Marshal(request)
					require.NoError(t, err)
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
					result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "client-cache", model)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "input.#").Int())
					require.Equal(t, "project instructions", gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String())
					require.Equal(t, "review files", gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String())
					require.NotContains(t, string(upstream.lastBody), openAICompatClaudeCodeTodoGuardMarker)
					require.Equal(t, withTools, gjson.GetBytes(upstream.lastBody, `tools.#(name=="TodoWrite")`).Exists())
					if accountType == AccountTypeOAuth {
						// 去掉提示后，Messages 桥接仍通过请求上下文保留缓存头行为。
						require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
						require.NotEmpty(t, upstream.lastReq.Header.Get("session_id"))
					} else {
						require.Equal(t, "client-cache", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
					}
				})
			}
		}
	}
}

// 停止生成提示不等于过滤用户正文；历史标签和客户端自带的提示应原样保留。
func TestPromptTransparencyPreservesClientSuppliedMarkerText(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark"} {
		t.Run(model, func(t *testing.T) {
			instructions := "client quotation: <sub2api-codex-image-generation> <sub2api-codex-spark-image-unsupported> <sub2api-claude-code-todo-guard> \n\t"
			body := map[string]any{"model": model, "instructions": instructions, "input": "hello"}
			applyCodexOAuthTransform(body, true, false)
			require.Equal(t, instructions, body["instructions"])
		})
	}
}

// 保留既有缺省提示策略：跳过默认模板时，Spark 分支也不额外填充能力提示。
func TestPromptTransparencySparkRespectsSkipDefaultInstructions(t *testing.T) {
	body := map[string]any{"model": "gpt-5.3-codex-spark", "input": "hello"}
	applyCodexOAuthTransformWithOptions(body, codexOAuthTransformOptions{SkipDefaultInstructions: true})
	require.NotContains(t, body, "instructions")
}

// 历史桥接标签仅作入站兼容识别，避免升级后丢失旧客户端的缓存桥接语义。
func TestPromptTransparencyLegacyMessagesBridgeRemainsReadable(t *testing.T) {
	body := map[string]any{
		"prompt_cache_key": "anthropic-metadata-session-1",
		"input": []any{map[string]any{
			"type": "message", "role": "developer",
			"content": []any{map[string]any{"type": "input_text", "text": openAICompatClaudeCodeTodoGuardMarker}},
		}},
	}
	// 常规 JSON 编码沿用缓存键识别，旧版未转义正文沿用 raw 标记识别。
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	require.True(t, isOpenAICompatMessagesBridgeBody(raw))
	require.True(t, isOpenAICompatMessagesBridgeBody([]byte(`{"input":[{"role":"developer","content":"<sub2api-claude-code-todo-guard>"}]}`)))
	require.True(t, isOpenAICompatMessagesBridgeRequestBody(body))
}
