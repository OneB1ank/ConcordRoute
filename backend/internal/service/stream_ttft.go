package service

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apicompat"
	"github.com/tidwall/gjson"
)

// anthropicStreamDataStartsVisibleOutput reports whether an Anthropic SSE
// payload contains the first output that a downstream client can consume.
// Lifecycle, usage, keepalive, and signature-only events are intentionally
// excluded from TTFT.
func anthropicStreamDataStartsVisibleOutput(data, eventType string) bool {
	if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
		return false
	}
	var event apicompat.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return false
	}
	if strings.TrimSpace(eventType) != "" && strings.TrimSpace(event.Type) == "" {
		event.Type = strings.TrimSpace(eventType)
	}
	return anthropicStreamEventStartsVisibleOutput(&event)
}

func anthropicStreamEventStartsVisibleOutput(event *apicompat.AnthropicStreamEvent) bool {
	if event == nil {
		return false
	}
	if event.Type != "content_block_delta" || event.Delta == nil {
		return false
	}
	switch event.Delta.Type {
	case "text_delta":
		return event.Delta.Text != ""
	case "thinking_delta":
		return event.Delta.Thinking != ""
	case "input_json_delta":
		return event.Delta.PartialJSON != ""
	case "":
		// A few legacy Anthropic-compatible providers omit delta.type while
		// still returning one of the standard content fields.
		return event.Delta.Text != "" || event.Delta.Thinking != "" || event.Delta.PartialJSON != ""
	default:
		return false
	}
}

func geminiStreamDataStartsVisibleOutput(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		return false
	}
	for _, part := range geminiResponseParts(response) {
		if text, ok := part["text"].(string); ok && text != "" {
			return true
		}
		if functionCall, ok := part["functionCall"].(map[string]any); ok && functionCall != nil {
			if name, _ := functionCall["name"].(string); strings.TrimSpace(name) != "" {
				return true
			}
			if args, ok := functionCall["args"]; ok && args != nil {
				return true
			}
		}
		if inlineData, ok := part["inlineData"].(map[string]any); ok && inlineData != nil {
			if encoded, _ := inlineData["data"].(string); encoded != "" {
				return true
			}
		}
	}
	return false
}

func geminiResponseParts(response map[string]any) []map[string]any {
	candidates, _ := response["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	out := make([]map[string]any, 0, len(parts))
	for _, value := range parts {
		if part, ok := value.(map[string]any); ok {
			out = append(out, part)
		}
	}
	return out
}

func openAIImageStreamDataStartsVisibleOutput(data []byte) bool {
	if openAIStreamDataStartsVisibleOutput(string(data), "") {
		return true
	}
	if len(data) == 0 {
		return false
	}
	for _, item := range gjson.ParseBytes(data).Get("data").Array() {
		if item.Get("b64_json").String() != "" || item.Get("url").String() != "" {
			return true
		}
	}
	return false
}

func anthropicSSEBytesStartsVisibleOutput(data []byte) bool {
	eventType := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if anthropicStreamDataStartsVisibleOutput(payload, eventType) {
				return true
			}
		}
	}
	return false
}

func responsesEventStartsVisibleOutput(event apicompat.ResponsesStreamEvent) bool {
	payload, err := json.Marshal(event)
	if err != nil {
		return false
	}
	return openAIStreamDataStartsVisibleOutput(string(payload), event.Type)
}

func chatCompletionsChunkStartsVisibleOutput(chunk apicompat.ChatCompletionsChunk) bool {
	payload, err := json.Marshal(chunk)
	if err != nil {
		return false
	}
	return openAIStreamDataStartsVisibleOutput(string(payload), "")
}

func recordFirstTokenMsAt(firstTokenMs **int, startTime, observedAt time.Time) {
	if firstTokenMs == nil || *firstTokenMs != nil {
		return
	}
	ms := int(observedAt.Sub(startTime).Milliseconds())
	if ms < 0 {
		ms = 0
	}
	*firstTokenMs = &ms
}

func recordFirstTokenMs(firstTokenMs **int, startTime time.Time) {
	recordFirstTokenMsAt(firstTokenMs, startTime, time.Now())
}
