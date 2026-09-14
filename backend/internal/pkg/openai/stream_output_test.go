package openai

import (
	"strings"
	"testing"
)

func TestStreamDataStartsVisibleOutputBytesMatchesStringAPI(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		eventType string
		want      bool
	}{
		{name: "created", data: `{"type":"response.created"}`},
		{name: "text delta", data: `{"type":"response.output_text.delta","delta":"hello"}`, want: true},
		{name: "empty delta", data: `{"type":"response.output_text.delta","delta":""}`},
		{name: "tool arguments", data: `{"type":"response.function_call_arguments.delta","delta":"{}"}`, want: true},
		{name: "tool search object arguments added", data: `{"type":"response.output_item.added","item":{"type":"tool_search_call","execution":"client","arguments":{"query":"synthetic tool"}}}`, want: true},
		{name: "tool search object arguments done", data: `{"type":"response.output_item.done","item":{"type":"tool_search_call","execution":"client","arguments":{"limit":1,"query":"synthetic tool"}}}`, want: true},
		{name: "tool search terminal output", data: `{"type":"response.completed","response":{"output":[{"type":"tool_search_call","arguments":{"query":"synthetic tool"}}]}}`, want: true},
		{name: "tool search legacy done output", data: `{"type":"response.done","response":{"output":[{"type":"tool_search_call","arguments":{"query":"synthetic tool"}}]}}`, want: true},
		{name: "tool search missing arguments", data: `{"type":"response.output_item.added","item":{"type":"tool_search_call","status":"in_progress"}}`},
		{name: "tool search empty object", data: `{"type":"response.output_item.added","item":{"type":"tool_search_call","arguments":{ }}}`},
		{name: "tool search empty completed object", data: `{"type":"response.completed","response":{"output":[{"type":"tool_search_call","status":"completed","arguments":{}}],"usage":{"output_tokens":28}}}`},
		{name: "tool search null", data: `{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":null}}`},
		{name: "tool search array", data: `{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":[{"query":"synthetic tool"}]}}`},
		{name: "tool search boolean", data: `{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":true}}`},
		{name: "tool search numeric", data: `{"type":"response.output_item.done","item":{"type":"tool_search_call","arguments":1}}`},
		{name: "function call object remains invalid", data: `{"type":"response.output_item.done","item":{"type":"function_call","arguments":{"query":"synthetic tool"}}}`},
		{name: "unknown item object remains structural", data: `{"type":"response.output_item.done","item":{"type":"unknown","arguments":{"query":"synthetic tool"}}}`},
		{name: "chat content", data: `{"choices":[{"delta":{"content":"hello"}}]}`, want: true},
		{name: "done", data: `[DONE]`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StreamDataStartsVisibleOutputBytes([]byte(tt.data), tt.eventType); got != tt.want {
				t.Fatalf("bytes API = %v, want %v", got, tt.want)
			}
			if got := StreamDataStartsVisibleOutput(tt.data, tt.eventType); got != tt.want {
				t.Fatalf("string API = %v, want %v", got, tt.want)
			}
		})
	}
}

func BenchmarkStreamDataStartsVisibleOutputBytes(b *testing.B) {
	data := []byte(`{"type":"response.output_text.delta","delta":"` +
		strings.Repeat("x", 128*1024) +
		`","response":{"id":"resp_bench","model":"gpt-5.6-sol"}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !StreamDataStartsVisibleOutputBytes(data, "response.output_text.delta") {
			b.Fatal("expected visible output")
		}
	}
}

func BenchmarkStreamDataStartsVisibleOutputString(b *testing.B) {
	data := []byte(`{"type":"response.output_text.delta","delta":"` +
		strings.Repeat("x", 128*1024) +
		`","response":{"id":"resp_bench","model":"gpt-5.6-sol"}}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !StreamDataStartsVisibleOutput(string(data), "response.output_text.delta") {
			b.Fatal("expected visible output")
		}
	}
}
