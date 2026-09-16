package openai

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 首语义事件允许空结构，但不把前导、错误、心跳和仅用量终态计为成功输出。
func TestStreamSemanticOutput(t *testing.T) {
	for _, tc := range []struct {
		name, payload, event string
		want                 bool
	}{
		{"created", `{"type":"response.created"}`, "", false},
		{"in_progress", `{"type":"response.in_progress"}`, "", false},
		{"heartbeat", `{"type":"ping"}`, "", false},
		{"done_marker", `[DONE]`, "", false},
		{"invalid_json", `{"type":"response.output_item.added"`, "", false},
		{"array", `[]`, "response.output_item.added", false},
		{"empty_reasoning", `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`, "", true},
		{"empty_message", `{"type":"response.output_item.added","item":{"type":"message","content":[]}}`, "", true},
		{"invalid_item", `{"type":"response.output_item.added","item":true}`, "", false},
		{"string_item", `{"type":"response.output_item.added","item":"{\"type\":\"reasoning\"}"}`, "", false},
		{"array_item", `{"type":"response.output_item.added","item":[{"type":"reasoning"}]}`, "", false},
		{"empty_part", `{"type":"response.reasoning_summary_part.added","part":{"type":"summary_text","text":""}}`, "", true},
		{"invalid_part", `{"type":"response.content_part.added","part":{}}`, "", false},
		{"empty_delta", `{"type":"response.output_text.delta","delta":""}`, "", true},
		{"invalid_delta", `{"type":"response.output_text.delta","delta":{}}`, "", false},
		{"summary", `{"type":"response.reasoning_summary_text.delta","delta":"summary"}`, "", true},
		{"event_header", `{"delta":""}`, " response.output_text.delta ", true},
		{"tool_arguments", `{"type":"response.function_call_arguments.delta","delta":"{}"}`, "", true},
		{"image", `{"type":"response.output_item.done","item":{"type":"image_generation_call","result":"image"}}`, "", true},
		{"usage_only", `{"type":"response.completed","response":{"usage":{"output_tokens":2}}}`, "", false},
		{"terminal_text", `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}}`, "", true},
		{"empty_incomplete", `{"type":"response.incomplete","response":{"output":[]}}`, "", false},
		{"failed", `{"type":"response.failed","response":{"error":{"code":"server_error"}}}`, "", false},
		{"error", `{"type":"error","error":{"message":"error"}}`, "", false},
		{"other_protocol", `{"type":"content_block_start"}`, "", false},
		{"unknown", `{"type":"response.unknown"}`, "", false},
		{"unknown_empty_delta", `{"type":"response.telemetry.delta","delta":""}`, "", false},
		{"unknown_text_delta", `{"type":"response.telemetry.delta","delta":"status"}`, "", false},
		{"error_empty_delta", `{"type":"response.error.delta","delta":""}`, "", false},
		{"error_text_delta", `{"type":"response.error.delta","delta":"upstream error"}`, "", false},
		{"refusal_delta", `{"type":"response.refusal.delta","delta":""}`, "", true},
		{"custom_tool_delta", `{"type":"response.custom_tool_call_input.delta","delta":""}`, "", true},
		{"audio_delta", `{"type":"response.output_audio.delta","delta":""}`, "", true},
		{"audio_transcript_delta", `{"type":"response.output_audio_transcript.delta","delta":""}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, StreamDataStartsSemanticOutputBytes([]byte(tc.payload), tc.event))
		})
	}
	empty := []byte(`{"type":"response.output_item.added","item":{"type":"reasoning","summary":[]}}`)
	require.True(t, StreamDataStartsSemanticOutputBytes(empty, ""))
	require.False(t, StreamDataStartsVisibleOutputBytes(empty, ""), "真实首内容分类仍保持原值")
}

// 大 item 只提取短类型字段，避免语义计时复制整块密文。
func BenchmarkStreamSemanticLargeItem(b *testing.B) {
	payload := []byte(`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"` +
		strings.Repeat("x", 128*1024) + `"}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !StreamDataStartsSemanticOutputBytes(payload, "response.output_item.added") {
			b.Fatal("未识别推理 item")
		}
	}
}
