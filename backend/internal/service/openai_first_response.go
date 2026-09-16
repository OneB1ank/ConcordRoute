package service

import (
	"io"
	"sync/atomic"
	"time"
)

// 首响应只在上游 Body.Read 返回非空数据时采样，不等待 SSE 行、JSON 或下游写出。
// 扫描协程可能与超时收尾并发；发布后数值不可变，读取侧取得独立快照。
type openAIFirstResponseReader struct {
	reader  io.Reader
	started time.Time
	first   atomic.Pointer[int]
}

func (r *openAIFirstResponseReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && r.first.Load() == nil {
		ms := max(0, int(time.Since(r.started).Milliseconds()))
		r.first.CompareAndSwap(nil, &ms)
	}
	return n, err
}

func (r *openAIFirstResponseReader) snapshot() *int {
	if value := r.first.Load(); value != nil {
		ms := *value
		return &ms
	}
	return nil
}

// WS 以收到的非空应用消息为首响应；本地保活和握手响应不经过该入口。
func recordOpenAIFirstResponseMs(first **int, started time.Time, payload []byte) {
	if len(payload) > 0 && *first == nil {
		recordFirstTokenMs(first, started)
	}
}
