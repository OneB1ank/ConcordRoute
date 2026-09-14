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
