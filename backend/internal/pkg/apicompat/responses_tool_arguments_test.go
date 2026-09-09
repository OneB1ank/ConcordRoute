package apicompat

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 同一流内交错的工具独立累积；done 补齐后仍可接收增量，已返回的片段保持不变。
func TestStreamingToolArgumentsInterleaved(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.OutputIndexToArguments = nil // 覆盖惰性初始化路径。
	for _, tool := range []struct {
		index int
		kind  string
	}{{2, "function_call"}, {7, "custom_tool_call"}} {
		ResponsesEventToChatChunks(&ResponsesStreamEvent{
			Type: "response.output_item.added", OutputIndex: tool.index,
			Item: &ResponsesOutput{Type: tool.kind, CallID: fmt.Sprint(tool.index), Name: "write_text"},
		}, state)
	}
	var emitted [2]strings.Builder
	var retained []ChatCompletionsChunk
	appendChunks := func(chunks []ChatCompletionsChunk) {
		retained = append(retained, chunks...)
		for _, chunk := range chunks {
			for _, call := range chunk.Choices[0].Delta.ToolCalls {
				require.NotNil(t, call.Index)
				_, _ = emitted[*call.Index].WriteString(call.Function.Arguments)
			}
		}
	}
	appendChunks(ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.function_call_arguments.delta", OutputIndex: 2, Delta: `{"text":"`,
	}, state))
	appendChunks(ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.custom_tool_call_input.done", OutputIndex: 7, Input: "工具\n",
	}, state))
	for i := 0; i < 4096; i++ {
		appendChunks(ResponsesEventToChatChunks(&ResponsesStreamEvent{
			Type: "response.function_call_arguments.delta", OutputIndex: 2, Delta: "abcdef0123456789",
		}, state))
		appendChunks(ResponsesEventToChatChunks(&ResponsesStreamEvent{
			Type: "response.custom_tool_call_input.delta", OutputIndex: 7, Delta: "内容🙂",
		}, state))
	}
	wantFunction := `{"text":"` + strings.Repeat("abcdef0123456789", 4096) + `"}`
	wantCustom := "工具\n" + strings.Repeat("内容🙂", 4096) + "\n结束"
	for _, done := range []ResponsesStreamEvent{
		{Type: "response.function_call_arguments.done", OutputIndex: 2, Arguments: wantFunction},
		{Type: "response.custom_tool_call_input.done", OutputIndex: 7, Input: wantCustom},
	} {
		appendChunks(ResponsesEventToChatChunks(&done, state))
		require.Empty(t, ResponsesEventToChatChunks(&done, state), "重复 done 不重发")
		bad := done
		bad.Arguments, bad.Input = "unrelated", "unrelated"
		require.Empty(t, ResponsesEventToChatChunks(&bad, state), "冲突 done 不覆盖已发送前缀")
		require.Empty(t, ResponsesEventToChatChunks(&done, state), "拒绝冲突后保留原进度")
	}
	require.Equal(t, wantFunction, emitted[0].String())
	require.Equal(t, wantCustom, emitted[1].String())
	// 扩容和 done 追加均不应修改之前已经交给调用者的片段。
	var replayed [2]strings.Builder
	for _, chunk := range retained {
		for _, call := range chunk.Choices[0].Delta.ToolCalls {
			_, _ = replayed[*call.Index].WriteString(call.Function.Arguments)
		}
	}
	require.Equal(t, wantFunction, replayed[0].String())
	require.Equal(t, wantCustom, replayed[1].String())
	require.Empty(t, ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.function_call_arguments.done", OutputIndex: 99, Arguments: "{}",
	}, state), "未知工具不创建下行调用")
}

// 与审查基准相同的 32 字节分片；加入 done，覆盖完整参数去重路径。
func benchmarkStreamingToolArguments(b *testing.B, size int) {
	args := `{"content":"` + strings.Repeat("x", size-len(`{"content":""}`)) + `"}`
	events := make([]ResponsesStreamEvent, 0, (len(args)+31)/32+1)
	for pos := 0; pos < len(args); pos += 32 {
		end := min(pos+32, len(args))
		events = append(events, ResponsesStreamEvent{
			Type: "response.function_call_arguments.delta", OutputIndex: 0, Delta: args[pos:end],
		})
	}
	events = append(events, ResponsesStreamEvent{
		Type: "response.function_call_arguments.done", OutputIndex: 0, Arguments: args,
	})
	b.ReportAllocs()
	b.SetBytes(int64(len(args)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		state := NewResponsesEventToChatState()
		ResponsesEventToChatChunks(&ResponsesStreamEvent{
			Type: "response.output_item.added",
			Item: &ResponsesOutput{Type: "function_call", CallID: "call_bench", Name: "write_text"},
		}, state)
		total := 0
		for i := range events {
			for _, chunk := range ResponsesEventToChatChunks(&events[i], state) {
				total += len(chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments)
			}
		}
		if total != len(args) {
			b.Fatalf("输出参数字节数错误：%d != %d", total, len(args))
		}
	}
}

// 只约束累计分配，不设机器相关的耗时阈值；旧的逐片拼接约分配 283 MB。
func TestStreamingToolArgumentsAllocationBudget(t *testing.T) {
	result := testing.Benchmark(func(b *testing.B) { benchmarkStreamingToolArguments(b, 128<<10) })
	t.Logf("128 KiB 参数：%d B/op，%d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
	if allocated := result.AllocedBytesPerOp(); allocated > 8<<20 {
		t.Fatalf("参数转换累计分配超出 8 MiB 预算：%d B/op", allocated)
	}
}

// 多个参数长度便于观察分配增长，避免小样本掩盖二次复制。
func BenchmarkStreamingToolArguments(b *testing.B) {
	for _, size := range []int{32 << 10, 64 << 10, 128 << 10} {
		b.Run(fmt.Sprintf("bytes_%d_chunk_32", size), func(b *testing.B) {
			benchmarkStreamingToolArguments(b, size)
		})
	}
}
