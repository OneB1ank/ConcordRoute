package latencytrace

import (
	"context"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 热路径只允许结束回调的少量固定分配，不为固定阶段名反复拼接字符串。
func TestTraceOperationAllocationBudget(t *testing.T) {
	r := New(time.Now())
	r.events = make([]Event, 0, 8)
	ctx := WithRecorder(context.Background(), r)
	allocs := testing.AllocsPerRun(1000, func() {
		r.events = r.events[:0]
		Start(ctx, "account_slot")(nil)
	})
	t.Logf("operation allocations=%g", allocs)
	require.LessOrEqual(t, allocs, float64(2))
	require.Equal(t, "account_slot_started", r.events[0].Phase)
	require.Equal(t, "account_slot_done", r.events[1].Phase)
}

// 固定阶段表必须保持原有白名单、结束幂等和未知名称丢弃的契约。
func TestTraceOperationNamesPreserveContract(t *testing.T) {
	for _, phase := range []string{
		"host_validation", "client_acquire", "dns", "tcp_connect", "tls", "proxy_tls",
		"proxy_tunnel", "tls_client_handshake", "user_slot", "account_selection",
		"account_slot", "retry_wait", "token_get", "token_cache_read", "token_refresh", "token_lock_wait",
		"", "PRIVATE_TOKEN", "token_get_started", "first_response_byte",
	} {
		t.Run(phase, func(t *testing.T) {
			r := New(time.Now())
			end := Start(WithRecorder(context.Background(), r), phase)
			end(context.Canceled)
			end(nil)
			events := r.Snapshot().Events
			if !allowed(phase+"_started") || !allowed(phase+"_done") {
				require.Empty(t, events)
				return
			}
			require.Len(t, events, 2)
			require.Equal(t, phase+"_started", events[0].Phase)
			require.Equal(t, phase+"_done", events[1].Phase)
			require.True(t, events[1].Failed)
		})
	}
}

func BenchmarkTraceOperation(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(map[bool]string{false: "off", true: "on"}[enabled], func(b *testing.B) {
			r := New(time.Now())
			r.events = make([]Event, 0, 8)
			ctx := context.Background()
			if enabled {
				ctx = WithRecorder(ctx, r)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.events = r.events[:0]
				Start(ctx, "account_slot")(nil)
			}
		})
	}
}

// 每次迭代拥有独立请求状态；不把饱和后丢事件的成本冒充完整请求成本。
func BenchmarkTraceRequest(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		for _, parallel := range []bool{false, true} {
			name := map[bool]string{false: "off", true: "on"}[enabled] + map[bool]string{false: "/serial", true: "/parallel"}[parallel]
			b.Run(name, func(b *testing.B) {
				request := func() {
					ctx := context.Background()
					var r *Recorder
					if enabled {
						r = New(time.Now())
						ctx = WithRecorder(ctx, r)
					}
					for _, phase := range []string{"account_selection", "account_slot", "token_get", "host_validation", "client_acquire"} {
						Start(ctx, phase)(nil)
					}
					ctx = WithHTTPTrace(ctx, nil)
					if trace := httptrace.ContextClientTrace(ctx); trace != nil {
						trace.GetConn("unused")
						trace.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true})
						trace.WroteHeaders()
						trace.WroteRequest(httptrace.WroteRequestInfo{})
						trace.GotFirstResponseByte()
					}
					_ = r.Snapshot()
				}
				b.ReportAllocs()
				if parallel {
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							request()
						}
					})
				} else {
					for i := 0; i < b.N; i++ {
						request()
					}
				}
			})
		}
	}
}
