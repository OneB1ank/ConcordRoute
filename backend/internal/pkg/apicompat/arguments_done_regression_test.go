package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 完整参数只补尚未输出的后缀，重复 done 不应再次发送。
func TestArgumentsDoneSuffixAndCustomTools(t *testing.T) {
	for _, custom := range []bool{false, true} {
		kind, deltaType, doneType := "function_call", "response.function_call_arguments.delta", "response.function_call_arguments.done"
		if custom {
			kind, deltaType, doneType = "custom_tool_call", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done"
		}
		t.Run(kind, func(t *testing.T) {
			state := NewResponsesEventToChatState()
			state.SentRole = true
			ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 3,
				Item: &ResponsesOutput{Type: kind, CallID: "call_a", Name: "lookup"}}, state)
			ResponsesEventToChatChunks(&ResponsesStreamEvent{Type: deltaType, OutputIndex: 3, Delta: `{"a":`}, state)
			event := &ResponsesStreamEvent{Type: doneType, OutputIndex: 3, Arguments: `{"a":1}`, Input: `{"a":1}`}
			chunks := ResponsesEventToChatChunks(event, state)
			require.Len(t, chunks, 1)
			require.Equal(t, "1}", chunks[0].Choices[0].Delta.ToolCalls[0].Function.Arguments)
			require.Empty(t, ResponsesEventToChatChunks(event, state))
			event.Arguments, event.Input = "unrelated", "unrelated"
			require.Empty(t, ResponsesEventToChatChunks(event, state), "不拼接互不兼容的完整参数")
		})
	}
}

// 终止 output 可能重排；相同下标不应覆盖不同 call_id 的工具参数。
func TestArgumentsDoneSupplementUsesCallIdentity(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	for i, id := range []string{"call_a", "call_b"} {
		acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: i,
			Item: &ResponsesOutput{Type: "function_call", CallID: id, Name: id}})
		acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: i, Arguments: id})
	}
	resp := &ResponsesResponse{Output: []ResponsesOutput{
		{Type: "function_call", CallID: "call_b"},
		{Type: "function_call", CallID: "call_a"},
		{Type: "function_call", CallID: "call_unknown"},
	}}
	acc.SupplementResponseOutput(resp)
	require.Equal(t, "call_b", resp.Output[0].Arguments)
	require.Equal(t, "call_a", resp.Output[1].Arguments)
	require.Empty(t, resp.Output[2].Arguments)
	resp.Output[0].Arguments = "authoritative"
	acc.SupplementResponseOutput(resp)
	require.Equal(t, "authoritative", resp.Output[0].Arguments)
}

// 自由文本工具使用 input，补齐后仍保持 Responses 的原始工具类型。
func TestArgumentsDoneSupplementCustomInput(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 2,
		Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_custom", Name: "lookup"}})
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.done", OutputIndex: 2, Input: "lookup query"})
	resp := &ResponsesResponse{Output: []ResponsesOutput{{Type: "custom_tool_call", CallID: "call_custom"}}}
	acc.SupplementResponseOutput(resp)
	require.Equal(t, "lookup query", resp.Output[0].Input)
	require.Empty(t, resp.Output[0].Arguments)
}

// 原生 Responses 的空终态重建也必须保留自定义工具类型，不能改写成普通函数调用。
func TestArgumentsDoneBuildOutputPreservesCustomType(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 0,
		Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_custom", Name: "lookup"}})
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.done", OutputIndex: 0, Input: "lookup query"})
	resp := &ResponsesResponse{Output: []ResponsesOutput{}}
	acc.SupplementResponseOutput(resp)
	require.Len(t, resp.Output, 1)
	require.Equal(t, "custom_tool_call", resp.Output[0].Type)
	require.Equal(t, "call_custom", resp.Output[0].CallID)
	require.Equal(t, "lookup", resp.Output[0].Name)
	require.Equal(t, "lookup query", resp.Output[0].Input)
	require.Empty(t, resp.Output[0].Arguments)
	chat := ResponsesToChatCompletions(resp, "")
	require.Len(t, chat.Choices[0].Message.ToolCalls, 1)
	require.JSONEq(t, `{"input":"lookup query"}`, chat.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

// Chat 的 custom 代理 schema 使用 input 字符串；即使原文看似 JSON，也应能逐字还原。
func TestArgumentsDoneCustomChatRoundTrip(t *testing.T) {
	for _, input := range []string{"lookup query", `{"query":"example"}`, "\"quoted\"\nnext line", ""} {
		resp := &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{
			{Type: "custom_tool_call", CallID: "call_custom", Name: "lookup", Input: input},
		}}
		chat := ResponsesToChatCompletions(resp, "")
		require.Len(t, chat.Choices, 1)
		require.Len(t, chat.Choices[0].Message.ToolCalls, 1)
		arguments := chat.Choices[0].Message.ToolCalls[0].Function.Arguments
		var proxyInput map[string]string
		require.NoError(t, json.Unmarshal([]byte(arguments), &proxyInput))
		require.Equal(t, map[string]string{"input": input}, proxyInput)
		restored := ChatCompletionsResponseToResponses(chat, "", map[string]bool{"lookup": true}, false, nil)
		require.Len(t, restored.Output, 1)
		require.Equal(t, "custom_tool_call", restored.Output[0].Type)
		require.Equal(t, input, restored.Output[0].Input)
	}
}

// 同一重建结果还供 Messages 使用；自由文本按现有 custom 工具桥接契约装入 input。
func TestArgumentsDoneCustomTypeMessagesCompatibility(t *testing.T) {
	acc := NewBufferedResponseAccumulator()
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.output_item.added", OutputIndex: 0,
		Item: &ResponsesOutput{Type: "custom_tool_call", CallID: "call_custom", Name: "lookup"}})
	acc.ProcessEvent(&ResponsesStreamEvent{Type: "response.custom_tool_call_input.done", OutputIndex: 0, Input: "lookup query"})
	resp := &ResponsesResponse{Status: "completed"}
	acc.SupplementResponseOutput(resp)
	message := ResponsesToAnthropic(resp, "")
	require.Len(t, message.Content, 1)
	require.Equal(t, "tool_use", message.Content[0].Type)
	require.JSONEq(t, `{"input":"lookup query"}`, string(message.Content[0].Input))
	_, err := json.Marshal(message)
	require.NoError(t, err)
}

// 自定义工具的对象输入保真；JSON 标量也按自由文本承载，避免生成非对象工具输入。
func TestArgumentsDoneCustomMessagesObjectAndScalar(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"query":"example"}`, `{"query":"example"}`},
		{`"text"`, `{"input":"\"text\""}`},
		{"null", `{"input":"null"}`},
		{"", `{"input":""}`},
	} {
		resp := &ResponsesResponse{Status: "completed", Output: []ResponsesOutput{
			{Type: "custom_tool_call", CallID: "call_test", Name: "lookup", Input: tc.input},
		}}
		message := ResponsesToAnthropic(resp, "")
		require.Equal(t, "tool_use", message.Content[0].Type)
		require.JSONEq(t, tc.want, string(message.Content[0].Input))
		_, err := json.Marshal(message)
		require.NoError(t, err)
	}
}
