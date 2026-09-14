package openai

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

func streamItemHasVisibleOutput(item gjson.Result) bool {
	for _, path := range []string{"arguments", "input", "result"} {
		value := item.Get(path)
		if value.Exists() && value.Type == gjson.String && value.String() != "" {
			return true
		}
	}
	for _, path := range []string{"content", "summary"} {
		for _, part := range item.Get(path).Array() {
			text := part.Get("text")
			transcript := part.Get("transcript")
			if (text.Exists() && text.Type == gjson.String && text.String() != "") ||
				(transcript.Exists() && transcript.Type == gjson.String && transcript.String() != "") {
				return true
			}
		}
	}
	return false
}

func chatCompletionsChunkHasVisibleOutput(root gjson.Result) bool {
	for _, choice := range root.Get("choices").Array() {
		delta := choice.Get("delta")
		for _, path := range []string{"content", "reasoning_content", "reasoning", "refusal", "audio.data", "audio.transcript"} {
			value := delta.Get(path)
			if value.Exists() && value.Type == gjson.String && value.String() != "" {
				return true
			}
		}
		for _, call := range delta.Get("tool_calls").Array() {
			arguments := call.Get("function.arguments")
			if arguments.Exists() && arguments.Type == gjson.String && arguments.String() != "" {
				return true
			}
		}
		arguments := delta.Get("function_call.arguments")
		if arguments.Exists() && arguments.Type == gjson.String && arguments.String() != "" {
			return true
		}
	}
	return false
}

// 结构进度可以提交当前 attempt 并解除首输出故障转移，但只有客户端可用内容才开始计算 TTFT。
//
// StreamDataStartsVisibleOutputBytes 是热路径版本。调用方已经持有 SSE 字节帧时，
// 直接处理 []byte 可避免额外字符串分配，适用于大型 Responses 事件的 HTTP/WS 路径。
func StreamDataStartsVisibleOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" || trimmed == "[DONE]" || !gjson.Valid(trimmed) {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = strings.TrimSpace(gjson.Get(trimmed, "type").String())
		if eventType == "" && gjson.Get(trimmed, "choices").Exists() {
			return chatCompletionsChunkHasVisibleOutput(gjson.Parse(trimmed))
		}
	}
	if strings.HasSuffix(eventType, ".delta") {
		delta := gjson.Get(trimmed, "delta")
		return delta.Exists() && delta.Type == gjson.String && delta.String() != ""
	}
	switch eventType {
	case "response.output_text.done",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.done",
		"response.audio_transcript.done":
		text := gjson.Get(trimmed, "text")
		return text.Exists() && text.Type == gjson.String && text.String() != ""
	case "response.function_call_arguments.done":
		arguments := gjson.Get(trimmed, "arguments")
		return arguments.Exists() && arguments.Type == gjson.String && arguments.String() != ""
	case "response.custom_tool_call_input.done":
		input := gjson.Get(trimmed, "input")
		return input.Exists() && input.Type == gjson.String && input.String() != ""
	case "response.image_generation_call.partial_image":
		partial := gjson.Get(trimmed, "partial_image_b64")
		return partial.Exists() && partial.Type == gjson.String && partial.String() != ""
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		part := gjson.Get(trimmed, "part")
		text := part.Get("text")
		transcript := part.Get("transcript")
		return (text.Exists() && text.Type == gjson.String && text.String() != "") ||
			(transcript.Exists() && transcript.Type == gjson.String && transcript.String() != "")
	case "response.output_item.added", "response.output_item.done":
		return streamItemHasVisibleOutput(gjson.Get(trimmed, "item"))
	case "response.completed", "response.done":
		for _, item := range gjson.Get(trimmed, "response.output").Array() {
			if streamItemHasVisibleOutput(item) {
				return true
			}
		}
	}
	return false
}

func StreamDataStartsVisibleOutputBytes(data []byte, eventType string) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || !gjson.ValidBytes(trimmed) {
		return false
	}
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = strings.TrimSpace(gjson.GetBytes(trimmed, "type").String())
		if eventType == "" && gjson.GetBytes(trimmed, "choices").Exists() {
			return chatCompletionsChunkHasVisibleOutput(gjson.ParseBytes(trimmed))
		}
	}
	if strings.HasSuffix(eventType, ".delta") {
		delta := gjson.GetBytes(trimmed, "delta")
		return delta.Exists() && delta.Type == gjson.String && delta.String() != ""
	}
	switch eventType {
	case "response.output_text.done",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.done",
		"response.audio_transcript.done":
		text := gjson.GetBytes(trimmed, "text")
		return text.Exists() && text.Type == gjson.String && text.String() != ""
	case "response.function_call_arguments.done":
		arguments := gjson.GetBytes(trimmed, "arguments")
		return arguments.Exists() && arguments.Type == gjson.String && arguments.String() != ""
	case "response.custom_tool_call_input.done":
		input := gjson.GetBytes(trimmed, "input")
		return input.Exists() && input.Type == gjson.String && input.String() != ""
	case "response.image_generation_call.partial_image":
		partial := gjson.GetBytes(trimmed, "partial_image_b64")
		return partial.Exists() && partial.Type == gjson.String && partial.String() != ""
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		part := gjson.GetBytes(trimmed, "part")
		text := part.Get("text")
		transcript := part.Get("transcript")
		return (text.Exists() && text.Type == gjson.String && text.String() != "") ||
			(transcript.Exists() && transcript.Type == gjson.String && transcript.String() != "")
	case "response.output_item.added", "response.output_item.done":
		return streamItemHasVisibleOutput(gjson.GetBytes(trimmed, "item"))
	case "response.completed", "response.done":
		for _, item := range gjson.GetBytes(trimmed, "response.output").Array() {
			if streamItemHasVisibleOutput(item) {
				return true
			}
		}
	}
	return false
}
