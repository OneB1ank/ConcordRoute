package service

import (
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 流式 Flush 会重复进入阶段标记；已规范化的固定名称不应每次分配字符串。
func TestTTFTStageNameHotPathNoAllocation(t *testing.T) {
	var result string
	allocs := testing.AllocsPerRun(1000, func() { result = normalizeTTFTStageName("first_downstream_flush") })
	require.Equal(t, "first_downstream_flush", result)
	t.Logf("stage name allocations=%g", allocs)
	require.Zero(t, allocs)
	for input, want := range map[string]string{
		"First-Byte!!": "first_byte", "a b中c": "abc", "___-9": "____9", "": "",
		strings.Repeat("a", 60): strings.Repeat("a", 48),
	} {
		require.Equal(t, want, normalizeTTFTStageName(input))
	}
}

func BenchmarkTTFTRepeatedFlush(b *testing.B) {
	c := &gin.Context{}
	InitTTFTStageTiming(c, true)
	mark := BeginTTFTUpstreamAttempt(c, 1)
	mark("first_downstream_flush")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mark("first_downstream_flush")
	}
}

// 覆盖入口、一次出站、128 次 Flush 标记和结束快照；无网络、模型或磁盘等待。
func BenchmarkTTFTDiagnosticsRequest(b *testing.B) {
	base, err := http.NewRequest(http.MethodPost, "http://example.invalid/v1/responses", nil)
	require.NoError(b, err)
	for _, enabled := range []bool{false, true} {
		b.Run(map[bool]string{false: "off", true: "on"}[enabled], func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c := &gin.Context{Request: base}
				InitTTFTStageTiming(c, enabled)
				for _, phase := range []string{"account_selection", "account_slot", "token_get"} {
					latencytrace.Start(c.Request.Context(), phase)(nil)
				}
				mark := BeginTTFTUpstreamAttempt(c, 1)
				req := withTTFTUpstreamTrace(c, c.Request, mark)
				for _, phase := range []string{"host_validation", "client_acquire"} {
					latencytrace.Start(req.Context(), phase)(nil)
				}
				if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
					trace.GetConn("unused")
					trace.GotConn(httptrace.GotConnInfo{Reused: true})
					trace.WroteHeaders()
					trace.WroteRequest(httptrace.WroteRequestInfo{})
					trace.GotFirstResponseByte()
				}
				for _, stage := range []string{"upstream_headers_received", "first_sse_event", "first_content_received", "first_content_flush_started", "first_content_flush_completed"} {
					mark(stage)
				}
				for j := 0; j < 128; j++ {
					mark("first_downstream_flush")
				}
				mark("stream_completed")
				_ = TTFTStageTimingSnapshot(c, time.Time{})
				_ = TTFTAttemptTimingSnapshots(c)
				_ = TTFTRequestOperationSnapshot(c)
			}
		})
	}
}
