package service

import (
	"sort"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// OpsTTFTStageTimingKey identifies the optional request phase sampler.
// The sampler is created only when gateway.ttft_diagnostics_enabled is true,
// so the normal hot path has no map allocation or extra logging.
const OpsTTFTStageTimingKey = "ops_ttft_stage_timing"

// TTFTStageTiming records monotonic request phase timestamps relative to the
// ingress start. Values are exposed as elapsed milliseconds, never payload or
// credential data.
type TTFTStageTiming struct {
	startedAt time.Time

	mu     sync.Mutex
	stages map[string]time.Time
}

// InitTTFTStageTiming enables phase sampling for a request and marks the
// ingress boundary. It is intentionally a no-op when disabled.
func InitTTFTStageTiming(c *gin.Context, enabled bool) {
	if c == nil || !enabled {
		return
	}
	timing := &TTFTStageTiming{
		startedAt: time.Now(),
		stages:    make(map[string]time.Time, 12),
	}
	c.Set(OpsTTFTStageTimingKey, timing)
	timing.Mark("request_received")
}

// MarkTTFTStage records the first occurrence of a named request phase.
// Duplicate callbacks (for example retries or multiple flushes) are ignored.
func MarkTTFTStage(c *gin.Context, stage string) {
	if c == nil {
		return
	}
	v, ok := c.Get(OpsTTFTStageTimingKey)
	if !ok {
		return
	}
	timing, ok := v.(*TTFTStageTiming)
	if !ok || timing == nil {
		return
	}
	timing.Mark(stage)
}

func (t *TTFTStageTiming) Mark(stage string) {
	if t == nil || t.startedAt.IsZero() {
		return
	}
	stage = normalizeTTFTStageName(stage)
	if stage == "" {
		return
	}
	t.mu.Lock()
	if _, exists := t.stages[stage]; !exists {
		t.stages[stage] = time.Now()
	}
	t.mu.Unlock()
}

// TTFTStageTimingSnapshot returns elapsed milliseconds from ingress for each
// observed phase. The returned map is a copy and can safely be logged.
func TTFTStageTimingSnapshot(c *gin.Context, endedAt time.Time) map[string]int64 {
	if c == nil {
		return nil
	}
	v, ok := c.Get(OpsTTFTStageTimingKey)
	if !ok {
		return nil
	}
	timing, ok := v.(*TTFTStageTiming)
	if !ok || timing == nil {
		return nil
	}
	return timing.Snapshot(endedAt)
}

func (t *TTFTStageTiming) Snapshot(endedAt time.Time) map[string]int64 {
	if t == nil || t.startedAt.IsZero() {
		return nil
	}
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	t.mu.Lock()
	stages := make(map[string]time.Time, len(t.stages))
	for name, at := range t.stages {
		stages[name] = at
	}
	t.mu.Unlock()
	result := make(map[string]int64, len(stages))
	for name, at := range stages {
		ms := at.Sub(t.startedAt).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		result[name] = ms
	}
	return result
}

// TTFTStageTimingNames returns stable ordering for diagnostics/tests.
func TTFTStageTimingNames(c *gin.Context) []string {
	snapshot := TTFTStageTimingSnapshot(c, time.Now())
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func normalizeTTFTStageName(stage string) string {
	if len(stage) > 48 {
		stage = stage[:48]
	}
	var b []byte
	for i := 0; i < len(stage); i++ {
		ch := stage[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			b = append(b, ch)
		case ch == '_' || ch == '-':
			b = append(b, '_')
		case ch >= 'A' && ch <= 'Z':
			b = append(b, ch+('a'-'A'))
		}
	}
	return string(b)
}
