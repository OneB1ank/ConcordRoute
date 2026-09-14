// Package latencytrace 记录有界、无正文和凭据的请求耗时事件；不改变出站协议。
package latencytrace

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

const maxEvents = 128

type contextKey struct{}

// Event 只暴露固定阶段名、相对时间和连接复用状态，不保存地址、错误文本或 TLS 身份。
type Event struct {
	Phase   string `json:"phase"`
	AtMS    int64  `json:"at_ms"`
	Failed  bool   `json:"failed,omitempty"`
	Reused  *bool  `json:"reused,omitempty"`
	WasIdle *bool  `json:"was_idle,omitempty"`
	IdleMS  int64  `json:"idle_ms,omitempty"`
}

type Snapshot struct {
	Events  []Event `json:"events"`
	Dropped int     `json:"dropped,omitempty"`
}

// Recorder 按发生顺序保存重复事件，避免 Happy Eyeballs、透明重试或重定向混成一对起止点。
type Recorder struct {
	started time.Time
	mu      sync.Mutex
	events  []Event
	dropped int
}

func New(started time.Time) *Recorder {
	return &Recorder{started: started}
}

func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(contextKey{}).(*Recorder)
	return recorder
}

// 固定白名单阻止未来调用方误把地址、令牌或错误文本当作指标名。
func allowed(phase string) bool {
	switch phase {
	case "host_validation_started", "host_validation_done", "client_acquire_started", "client_acquire_done",
		"connection_get", "connection_got", "dns_started", "dns_done", "tcp_connect_started", "tcp_connect_done",
		"tls_started", "tls_done", "proxy_tls_started", "proxy_tls_done",
		"proxy_tunnel_started", "proxy_tunnel_done", "tls_client_handshake_started", "tls_client_handshake_done",
		"request_headers_written", "request_written", "first_response_byte",
		"user_slot_started", "user_slot_done", "account_selection_started", "account_selection_done",
		"account_slot_started", "account_slot_done", "retry_wait_started", "retry_wait_done",
		"token_get_started", "token_get_done", "token_cache_read_started", "token_cache_read_done",
		"token_cache_hit", "token_cache_miss", "token_refresh_started", "token_refresh_done",
		"token_lock_wait_started", "token_lock_wait_done":
		return true
	}
	return false
}

func (r *Recorder) record(event Event) {
	if r == nil || !allowed(event.Phase) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) >= maxEvents {
		r.dropped++
		return
	}
	event.AtMS = max(0, time.Since(r.started).Milliseconds())
	r.events = append(r.events, event)
}

func Mark(ctx context.Context, phase string, err error) {
	FromContext(ctx).record(Event{Phase: phase, Failed: err != nil})
}

func noopEnd(error) {}

// 固定名称直接返回常量，避免在请求热路径重复拼接起止后缀；与事件白名单一致。
func operationPhases(phase string) (string, string) {
	switch phase {
	case "host_validation":
		return "host_validation_started", "host_validation_done"
	case "client_acquire":
		return "client_acquire_started", "client_acquire_done"
	case "dns":
		return "dns_started", "dns_done"
	case "tcp_connect":
		return "tcp_connect_started", "tcp_connect_done"
	case "tls":
		return "tls_started", "tls_done"
	case "proxy_tls":
		return "proxy_tls_started", "proxy_tls_done"
	case "proxy_tunnel":
		return "proxy_tunnel_started", "proxy_tunnel_done"
	case "tls_client_handshake":
		return "tls_client_handshake_started", "tls_client_handshake_done"
	case "user_slot":
		return "user_slot_started", "user_slot_done"
	case "account_selection":
		return "account_selection_started", "account_selection_done"
	case "account_slot":
		return "account_slot_started", "account_slot_done"
	case "retry_wait":
		return "retry_wait_started", "retry_wait_done"
	case "token_get":
		return "token_get_started", "token_get_done"
	case "token_cache_read":
		return "token_cache_read_started", "token_cache_read_done"
	case "token_refresh":
		return "token_refresh_started", "token_refresh_done"
	case "token_lock_wait":
		return "token_lock_wait_started", "token_lock_wait_done"
	default:
		return "", ""
	}
}

// Start 的结束函数只记一次；关闭诊断时没有计时、上下文包装或采样分配。
func Start(ctx context.Context, phase string) func(error) {
	r := FromContext(ctx)
	if r == nil {
		return noopEnd
	}
	started, done := operationPhases(phase)
	if started == "" {
		return noopEnd
	}
	r.record(Event{Phase: started})
	var once sync.Once
	return func(err error) {
		once.Do(func() { r.record(Event{Phase: done, Failed: err != nil}) })
	}
}

func (r *Recorder) Snapshot() *Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := &Snapshot{Events: append([]Event(nil), r.events...), Dropped: r.dropped}
	for i := range result.Events {
		if p := result.Events[i].Reused; p != nil {
			value := *p
			result.Events[i].Reused = &value
		}
		if p := result.Events[i].WasIdle; p != nil {
			value := *p
			result.Events[i].WasIdle = &value
		}
	}
	return result
}

// WithHTTPTrace 保留已有 httptrace 回调，仅采集实际发生的事件。远端 DNS 和复用连接没有
// 对应本地阶段时保持缺省，不伪造 0ms；首响应字节可能是信息响应，不等于模型首内容。
func WithHTTPTrace(ctx context.Context, firstByte func()) context.Context {
	r := FromContext(ctx)
	if r == nil {
		return ctx
	}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { Mark(ctx, "connection_get", nil) },
		GotConn: func(info httptrace.GotConnInfo) {
			reused, idle := info.Reused, info.WasIdle
			r.record(Event{Phase: "connection_got", Reused: &reused, WasIdle: &idle, IdleMS: info.IdleTime.Milliseconds()})
		},
		DNSStart:          func(httptrace.DNSStartInfo) { Mark(ctx, "dns_started", nil) },
		DNSDone:           func(info httptrace.DNSDoneInfo) { Mark(ctx, "dns_done", info.Err) },
		ConnectStart:      func(string, string) { Mark(ctx, "tcp_connect_started", nil) },
		ConnectDone:       func(_, _ string, err error) { Mark(ctx, "tcp_connect_done", err) },
		TLSHandshakeStart: func() { Mark(ctx, "tls_started", nil) },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { Mark(ctx, "tls_done", err) },
		WroteHeaders:      func() { Mark(ctx, "request_headers_written", nil) },
		WroteRequest:      func(info httptrace.WroteRequestInfo) { Mark(ctx, "request_written", info.Err) },
		GotFirstResponseByte: func() {
			Mark(ctx, "first_response_byte", nil)
			if firstByte != nil {
				firstByte()
			}
		},
	}
	return httptrace.WithClientTrace(ctx, trace)
}
