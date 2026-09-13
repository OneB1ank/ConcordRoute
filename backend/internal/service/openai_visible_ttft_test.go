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
	"github.com/TokenFlux/TokenRouter/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIVisibleOutputClassification(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		eventType string
		want      bool
	}{
		{name: "keepalive", data: `{"type":"keepalive"}`, want: false},
		{name: "created", data: `{"type":"response.created"}`, want: false},
		{name: "empty output item", data: `{"type":"response.output_item.added","item":{"id":"item_test","type":"reasoning","summary":[]}}`, want: false},
		{name: "empty delta", data: `{"type":"response.output_text.delta","delta":""}`, want: false},
		{name: "text delta", data: `{"type":"response.output_text.delta","delta":"test output"}`, want: true},
		{name: "tool arguments", data: `{"type":"response.function_call_arguments.delta","delta":"{}"}`, want: true},
		{name: "object delta is not visible output", data: `{"type":"response.output_text.delta","delta":{}}`, want: false},
		{name: "numeric delta is not visible output", data: `{"type":"response.output_text.delta","delta":1}`, want: false},
		{name: "partial image", data: `{"type":"response.image_generation_call.partial_image","partial_image_b64":"dGVzdA=="}`, want: true},
		{name: "completed image item", data: `{"type":"response.output_item.done","item":{"id":"item_test","type":"image_generation_call","result":"dGVzdA=="}}`, want: true},
		{name: "object tool arguments are not visible output", data: `{"type":"response.output_item.done","item":{"type":"function_call","arguments":{}}}`, want: false},
		{name: "object content text is not visible output", data: `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":{}}]}}`, want: false},
		{name: "empty completed", data: `{"type":"response.completed","response":{"id":"resp_test","output":[]}}`, want: false},
		{name: "completed with output usage only", data: `{"type":"response.completed","response":{"id":"resp_test","usage":{"input_tokens":1,"output_tokens":2}}}`, want: false},
		{name: "completed with text", data: `{"type":"response.completed","response":{"id":"resp_test","output":[{"type":"message","content":[{"type":"output_text","text":"test output"}]}]}}`, want: true},
		{name: "done marker", data: `[DONE]`, want: false},
		{name: "chat role-only chunk", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`, want: false},
		{name: "chat empty delta", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`, want: false},
		{name: "chat text delta", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`, want: true},
		{name: "chat tool arguments", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}`, want: true},
		{name: "chat object tool arguments", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":{}}}]},"finish_reason":null}]}`, want: false},
		{name: "chat refusal", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"refusal":"blocked"},"finish_reason":null}]}`, want: true},
		{name: "chat audio transcript", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"audio":{"transcript":"hello"}},"finish_reason":null}]}`, want: true},
		{name: "chat usage-only chunk", data: `{"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0}}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, openAIStreamDataStartsVisibleOutput(tt.data, tt.eventType))
		})
	}
}

func TestRecordFirstTokenMsAtClampsAndDoesNotOverwrite(t *testing.T) {
	start := time.Unix(100, 0)
	first := (*int)(nil)
	recordFirstTokenMsAt(&first, start, start.Add(-time.Millisecond))
	require.NotNil(t, first)
	require.Zero(t, *first)

	value := 42
	existing := valuePtr(value)
	recordFirstTokenMsAt(&existing, start, start.Add(time.Second))
	require.NotNil(t, existing)
	require.Equal(t, 42, *existing)
}

func valuePtr(value int) *int { return &value }

func TestCrossProtocolVisibleOutputClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		want bool
	}{
		{name: "anthropic message start", data: `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`},
		{name: "anthropic text delta", data: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, want: true},
		{name: "anthropic signature delta", data: `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"sig"}}`},
		{name: "anthropic usage delta", data: `{"type":"message_delta","usage":{"output_tokens":1}}`},
		{name: "gemini usage only", data: `{"usageMetadata":{"promptTokenCount":1}}`},
		{name: "gemini finish only", data: `{"candidates":[{"finishReason":"STOP"}]}`},
		{name: "gemini text part", data: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, want: true},
		{name: "gemini function call", data: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, want: true},
		{name: "gemini inline image", data: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"ZmFrZQ=="}}]}}]}`, want: true},
		{name: "image lifecycle", data: `{"type":"response.created","response":{"id":"resp_test"}}`},
		{name: "image partial", data: `{"type":"response.image_generation_call.partial_image","partial_image_b64":"ZmFrZQ=="}`, want: true},
		{name: "image result array", data: `{"data":[{"b64_json":"ZmFrZQ=="}]}`, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if strings.HasPrefix(tt.name, "anthropic") {
				require.Equal(t, tt.want, anthropicStreamDataStartsVisibleOutput(tt.data, ""))
				return
			}
			if strings.HasPrefix(tt.name, "gemini") {
				require.Equal(t, tt.want, geminiStreamDataStartsVisibleOutput([]byte(tt.data)))
				return
			}
			require.Equal(t, tt.want, openAIImageStreamDataStartsVisibleOutput([]byte(tt.data)))
		})
	}
}

func TestAnthropicBridgeTTFTStartsAtVisibleContent(t *testing.T) {
	for _, name := range []string{"responses", "chat_completions", "passthrough", "generic"} {
		t.Run(name, func(t *testing.T) {
			resp, writerDone := syntheticAnthropicStream(t, 120*time.Millisecond, true)
			started := time.Now()
			account := &Account{ID: 1, Name: "account_test", Platform: PlatformAnthropic}
			svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			var got *ForwardResult
			var err error
			switch name {
			case "responses":
				got, err = svc.handleResponsesStreamingResponse(resp, c, "test-model", "test-model", nil, started, apicompat.ResponsesClientToolMapping{})
			case "chat_completions":
				got, err = svc.handleCCStreamingFromAnthropic(resp, c, "test-model", "test-model", nil, started, true)
			case "passthrough":
				var passthrough *streamingResult
				passthrough, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account, started, "test-model")
				if passthrough != nil {
					got = &ForwardResult{FirstTokenMs: passthrough.firstTokenMs}
				}
			case "generic":
				var generic *streamingResult
				generic, err = svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test-model", "test-model", false)
				if generic != nil {
					got = &ForwardResult{FirstTokenMs: generic.firstTokenMs}
				}
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.NotNil(t, got.FirstTokenMs)
			require.GreaterOrEqual(t, *got.FirstTokenMs, 100)
			select {
			case <-writerDone:
			case <-time.After(time.Second):
				t.Fatal("synthetic Anthropic upstream writer did not exit")
			}
		})
	}
}

func TestAnthropicBridgeTTFTIgnoresMetadataOnlyStream(t *testing.T) {
	for _, name := range []string{"responses", "chat_completions", "passthrough", "generic"} {
		t.Run(name, func(t *testing.T) {
			resp, writerDone := syntheticAnthropicStream(t, 20*time.Millisecond, false)
			started := time.Now()
			account := &Account{ID: 1, Name: "account_test", Platform: PlatformAnthropic}
			svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

			var got *ForwardResult
			var err error
			switch name {
			case "responses":
				got, err = svc.handleResponsesStreamingResponse(resp, c, "test-model", "test-model", nil, started, apicompat.ResponsesClientToolMapping{})
			case "chat_completions":
				got, err = svc.handleCCStreamingFromAnthropic(resp, c, "test-model", "test-model", nil, started, true)
			case "passthrough":
				var result *streamingResult
				result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account, started, "test-model")
				if result != nil {
					got = &ForwardResult{FirstTokenMs: result.firstTokenMs}
				}
			case "generic":
				var result *streamingResult
				result, err = svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test-model", "test-model", false)
				if result != nil {
					got = &ForwardResult{FirstTokenMs: result.firstTokenMs}
				}
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Nil(t, got.FirstTokenMs)
			<-writerDone
		})
	}
}

func syntheticAnthropicStream(t *testing.T, visibleDelay time.Duration, includeVisible bool) (*http.Response, <-chan struct{}) {
	t.Helper()
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = writer.Close() }()
		write := func(event, data string) {
			_, _ = io.WriteString(writer, "event: "+event+"\n")
			_, _ = io.WriteString(writer, "data: "+data+"\n\n")
		}
		write("message_start", `{"type":"message_start","message":{"id":"msg_test","model":"test-model","usage":{"input_tokens":1}}}`)
		write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		time.Sleep(visibleDelay)
		if includeVisible {
			write("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
		}
		write("content_block_stop", `{"type":"content_block_stop","index":0}`)
		write("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
		write("message_stop", `{"type":"message_stop"}`)
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"req_test"}}, Body: reader}, done
}

func TestOpenAIResponsesTTFTStartsAtVisibleOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			result := runSyntheticVisibleTTFTStream(t, passthrough, 120*time.Millisecond, 0,
				`{"type":"response.output_text.delta","delta":"test output"}`)
			require.NotNil(t, result.firstTokenMs)
			require.GreaterOrEqual(t, *result.firstTokenMs, 100)
		})
	}
}

func TestOpenAIResponsesTTFTStartsAtCompletedImage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			result := runSyntheticVisibleTTFTStream(t, passthrough, 120*time.Millisecond, 0,
				`{"type":"response.output_item.done","item":{"id":"item_test","type":"image_generation_call","result":"dGVzdA=="}}`)
			require.NotNil(t, result.firstTokenMs)
			require.GreaterOrEqual(t, *result.firstTokenMs, 100)
		})
	}
}

func TestOpenAINativeProgressDisarmsTimeoutWithoutStartingTTFT(t *testing.T) {
	result := runSyntheticVisibleTTFTStream(t, false, 1200*time.Millisecond, 1,
		`{"type":"response.output_text.delta","delta":"test output"}`)
	require.NotNil(t, result.firstTokenMs)
	require.GreaterOrEqual(t, *result.firstTokenMs, 1100)
}

func runSyntheticVisibleTTFTStream(t *testing.T, passthrough bool, visibleDelay time.Duration, timeoutSeconds int, visibleEvent string) *openaiStreamingResult {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize:                     defaultMaxLineSize,
		OpenAIFirstOutputTimeoutSeconds: timeoutSeconds,
	}}}
	reader, writer := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = writer.Close() }()
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\"}}\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_test\",\"type\":\"reasoning\",\"summary\":[]}}\n\n")
		time.Sleep(visibleDelay)
		_, _ = io.WriteString(writer, "data: "+visibleEvent+"\n\n")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	InitTTFTStageTiming(c, true)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	account := &Account{ID: 1, Name: "account_test", Platform: PlatformOpenAI}
	started := time.Now()

	var result *openaiStreamingResult
	var err error
	if passthrough {
		var passthroughResult *openaiStreamingResultPassthrough
		passthroughResult, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, started, "test-model", "test-model")
		if passthroughResult != nil {
			result = &openaiStreamingResult{firstTokenMs: passthroughResult.firstTokenMs}
		}
	} else {
		result, err = svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test-model", "test-model")
	}
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, recorder.Body.String(), `"type":"response.output_item.added"`)
	require.Contains(t, recorder.Body.String(), visibleEvent)
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("synthetic upstream writer did not exit")
	}
	stages := TTFTStageTimingSnapshot(c, time.Now())
	t.Logf("ttft stages (ms): %v", stages)
	require.Contains(t, stages, "first_sse_event")
	require.Contains(t, stages, "first_visible_output")
	require.Contains(t, stages, "first_downstream_flush")
	require.Contains(t, stages, "stream_completed")
	return result
}
