package openai

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// 完整内容片段与增量一样可以提供首内容；拒绝文本不是空的结构通知。
func streamPartHasVisibleOutput(part gjson.Result) bool {
	for _, path := range []string{"text", "transcript"} {
		value := part.Get(path)
		if value.Type == gjson.String && value.String() != "" {
			return true
		}
	}
	if part.Get("type").String() == "refusal" {
		value := part.Get("refusal")
		return value.Type == gjson.String && value.String() != ""
	}
	return false
}

func streamItemHasVisibleOutput(item gjson.Result) bool {
	for _, path := range []string{"arguments", "input", "result"} {
		value := item.Get(path)
		if value.Exists() && value.Type == gjson.String && value.String() != "" {
			return true
		}
		if path == "arguments" && value.IsObject() && item.Get("type").String() == "tool_search_call" {
			// 工具发现调用使用对象参数；只识别已填充的对象，不把空占位或其它工具的异常类型算作首内容。
			hasArguments := false
			value.ForEach(func(_, _ gjson.Result) bool {
				hasArguments = true
				return false
			})
			if hasArguments {
				return true
			}
		}
	}
	for _, path := range []string{"content", "summary"} {
		for _, part := range item.Get(path).Array() {
			if streamPartHasVisibleOutput(part) {
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
	return streamDataStartsVisibleOutput(streamOutputJSON{text: trimmed}, eventType)
}

func StreamDataStartsVisibleOutputBytes(data []byte, eventType string) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || !gjson.ValidBytes(trimmed) {
		return false
	}
	return streamDataStartsVisibleOutput(streamOutputJSON{bytes: trimmed}, eventType)
}

// 两种原始载体共享事件语义，不为复用逻辑把大字节帧整体转换成字符串。
type streamOutputJSON struct {
	text  string
	bytes []byte
}

func (data streamOutputJSON) get(path string) gjson.Result {
	if data.bytes != nil {
		return gjson.GetBytes(data.bytes, path)
	}
	return gjson.Get(data.text, path)
}

func (data streamOutputJSON) parse() gjson.Result {
	if data.bytes != nil {
		return gjson.ParseBytes(data.bytes)
	}
	return gjson.Parse(data.text)
}

func streamDataStartsVisibleOutput(data streamOutputJSON, eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		eventType = strings.TrimSpace(data.get("type").String())
		if eventType == "" && data.get("choices").Exists() {
			return chatCompletionsChunkHasVisibleOutput(data.parse())
		}
	}
	if strings.HasSuffix(eventType, ".delta") {
		delta := data.get("delta")
		return delta.Exists() && delta.Type == gjson.String && delta.String() != ""
	}
	switch eventType {
	case "response.output_text.done",
		"response.reasoning_summary_text.done",
		"response.reasoning_text.done",
		"response.audio_transcript.done":
		text := data.get("text")
		return text.Exists() && text.Type == gjson.String && text.String() != ""
	case "response.function_call_arguments.done":
		arguments := data.get("arguments")
		return arguments.Exists() && arguments.Type == gjson.String && arguments.String() != ""
	case "response.refusal.done":
		refusal := data.get("refusal")
		return refusal.Type == gjson.String && refusal.String() != ""
	case "response.custom_tool_call_input.done":
		input := data.get("input")
		return input.Exists() && input.Type == gjson.String && input.String() != ""
	case "response.image_generation_call.partial_image":
		partial := data.get("partial_image_b64")
		return partial.Exists() && partial.Type == gjson.String && partial.String() != ""
	case "response.content_part.added", "response.content_part.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return streamPartHasVisibleOutput(data.get("part"))
	case "response.output_item.added", "response.output_item.done":
		return streamItemHasVisibleOutput(data.get("item"))
	case "response.completed", "response.done", "response.incomplete":
		// 未完成响应也可能带部分正文；只看实际 output，不用状态或 output_tokens 推定首字。
		for _, item := range data.get("response.output").Array() {
			if streamItemHasVisibleOutput(item) {
				return true
			}
		}
	}
	return false
}
