package openai_ws_v2

import (
	"sync"
	"time"
)

// 展示计时单独关联请求与响应，不改变中继既有的生命周期、首内容和耗时起点。
// 请求写侧与响应读侧并发；只保存计时，不保留帧、凭据或正文。
type relayFirstResponse struct {
	mu      sync.Mutex
	pending []*relayResponseSample
	byID    map[string]*relayResponseSample
	active  *relayResponseSample
	first   *int
	settled time.Duration
}

type relayResponseSample struct {
	started time.Time
	first   *int
	id      string
}

func (r *relayFirstResponse) begin(started time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, &relayResponseSample{started: started})
}

// 非空应用帧即可采样；无 ID 帧归当前轮次，后来的 ID 只补关联、不重新计时。
func (r *relayFirstResponse) observe(kind, id string, now time.Time) *int {
	terminal := isTerminalEvent(kind)
	r.mu.Lock()
	defer r.mu.Unlock()
	sample := r.active
	if id != "" {
		sample = r.byID[id]
		if sample == nil && r.active != nil && r.active.id == "" {
			sample = r.active
		}
	}
	newSample := sample == nil && len(r.pending) > 0
	if newSample {
		sample = r.pending[0]
		r.pending[0] = nil
		r.pending = r.pending[1:]
		if len(r.pending) == 0 {
			r.pending = nil
		}
	}
	if sample == nil {
		return nil
	}
	if id != "" && sample.id == "" {
		if r.byID == nil {
			r.byID = make(map[string]*relayResponseSample)
		}
		sample.id = id
		r.byID[id] = sample
	}
	if newSample || r.active == nil {
		r.active = sample
	}
	recordRelayFirstTokenMs(&sample.first, sample.started, now, true)
	if r.first == nil {
		r.first = openAIWSRelayCloneIntPtr(sample.first)
	}
	// 采样发布后不再修改；内部复用指针，避免每个后续帧都分配一次，输出边界再复制。
	result := sample.first
	if terminal {
		delete(r.byID, sample.id)
		if r.active == sample {
			r.active = nil
		}
		r.settled = max(time.Duration(0), now.Sub(sample.started))
	}
	return result
}

// 展示用总耗时与首响应共用请求起点，避免旧 WS 起点造成“首响应大于总耗时”。
func (r *relayFirstResponse) settledDuration() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled
}

func (r *relayFirstResponse) snapshot() *int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return openAIWSRelayCloneIntPtr(r.first)
}
