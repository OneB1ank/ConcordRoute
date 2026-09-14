package service

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/gin-gonic/gin"
)

// 阶段采样仅在诊断开关打开时分配状态，不向正常请求额外写入日志。
const OpsTTFTStageTimingKey = "ops_ttft_stage_timing"

const maxTTFTDiagnosticAttempts = 64

// TTFTStageTiming 分离请求级前置阶段与各次上游尝试，所有时间均相对网关入口。
type TTFTStageTiming struct {
	startedAt time.Time

	mu           sync.Mutex
	stages       map[string]time.Time
	attempts     []*ttftAttemptTiming
	attemptCount int
	operations   *latencytrace.Recorder
}

type ttftAttemptTiming struct {
	number    int
	accountID int64
	stages    map[string]time.Time
	transport *latencytrace.Recorder
}

// TTFTAttemptSnapshot 不包含正文、凭据或证明，仅用于关联同一请求的不同尝试。
type TTFTAttemptSnapshot struct {
	Attempt   int                    `json:"attempt"`
	AccountID int64                  `json:"account_id,omitempty"`
	StagesMS  map[string]int64       `json:"stages_ms"`
	Transport *latencytrace.Snapshot `json:"transport,omitempty"`
}

// InitTTFTStageTiming 开启请求级阶段采样；关闭时保持无状态。
func InitTTFTStageTiming(c *gin.Context, enabled bool) {
	if c == nil || !enabled {
		return
	}
	timing := &TTFTStageTiming{
		startedAt: time.Now(),
		stages:    make(map[string]time.Time, 12),
	}
	// 请求级操作保留重复排队/重试；向下只传播采样上下文，不追加任何 HTTP Header。
	timing.operations = latencytrace.New(timing.startedAt)
	if c.Request != nil {
		c.Request = c.Request.WithContext(latencytrace.WithRecorder(c.Request.Context(), timing.operations))
	}
	c.Set(OpsTTFTStageTimingKey, timing)
	timing.Mark("request_received")
}

// MarkTTFTStage 在所属请求或尝试内记录首次阶段，不把不同尝试拼成一条时间线。
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
	defer t.mu.Unlock()
	stages := t.stages
	if isTTFTAttemptStage(stage) {
		if len(t.attempts) == 0 {
			t.newAttemptLocked(0)
		}
		stages = t.attempts[len(t.attempts)-1].stages
	}
	if _, exists := stages[stage]; !exists {
		stages[stage] = time.Now()
	}
}

// TTFTStageTimingSnapshot 保留平面兼容格式，但上游阶段只来自最后一次尝试。
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

func (t *TTFTStageTiming) Snapshot(_ time.Time) map[string]int64 {
	if t == nil || t.startedAt.IsZero() {
		return nil
	}
	t.mu.Lock()
	stages := make(map[string]time.Time, len(t.stages))
	for name, at := range t.stages {
		stages[name] = at
	}
	if len(t.attempts) > 0 {
		for name, at := range t.attempts[len(t.attempts)-1].stages {
			stages[name] = at
		}
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

// TTFTStageTimingNames 返回稳定排序，供诊断和测试使用。
func TTFTStageTimingNames(c *gin.Context) []string {
	snapshot := TTFTStageTimingSnapshot(c, time.Now())
	names := make([]string, 0, len(snapshot))
	for name := range snapshot {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func ttftTimingFromContext(c *gin.Context) *TTFTStageTiming {
	if c == nil {
		return nil
	}
	value, _ := c.Get(OpsTTFTStageTimingKey)
	timing, _ := value.(*TTFTStageTiming)
	return timing
}

func isTTFTAttemptStage(stage string) bool {
	switch stage {
	case "upstream_do_started", "upstream_headers_received", "first_upstream_byte",
		"stream_started", "first_sse_event", "first_content_received",
		"first_content_flush_started", "first_content_flush_completed",
		"first_visible_output", "first_downstream_flush", "stream_completed":
		return true
	}
	return false
}

// 调用方持锁；数量有界，极端重试保留最近若干尝试且编号保持单调。
func (t *TTFTStageTiming) newAttemptLocked(accountID int64) *ttftAttemptTiming {
	t.attemptCount++
	attempt := &ttftAttemptTiming{number: t.attemptCount, accountID: accountID, stages: make(map[string]time.Time), transport: latencytrace.New(t.startedAt)}
	if len(t.attempts) == maxTTFTDiagnosticAttempts {
		copy(t.attempts, t.attempts[1:])
		t.attempts = t.attempts[:len(t.attempts)-1]
	}
	t.attempts = append(t.attempts, attempt)
	return attempt
}

func noopTTFTStage(string) {}

// 回调固定绑定到某次尝试；旧连接迟到的 trace 回调不会写入新尝试。
func (t *TTFTStageTiming) attemptMarker(attempt *ttftAttemptTiming) func(string) {
	return func(stage string) {
		stage = normalizeTTFTStageName(stage)
		if stage == "" {
			return
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if _, exists := attempt.stages[stage]; !exists {
			attempt.stages[stage] = time.Now()
		}
	}
}

// BeginTTFTUpstreamAttempt 在每次实际 Do 前建立新尝试，包括同账号的内部重试。
func BeginTTFTUpstreamAttempt(c *gin.Context, accountID int64) func(string) {
	timing := ttftTimingFromContext(c)
	if timing == nil {
		return noopTTFTStage
	}
	timing.mu.Lock()
	attempt := timing.newAttemptLocked(accountID)
	attempt.stages["upstream_do_started"] = time.Now()
	timing.mu.Unlock()
	return timing.attemptMarker(attempt)
}

// 流处理承接 Do 的尝试；直接进入流处理的桥接或测试路径按流建立隔离状态。
func beginTTFTStream(c *gin.Context, account *Account) func(string) {
	timing := ttftTimingFromContext(c)
	if timing == nil {
		return noopTTFTStage
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	timing.mu.Lock()
	var attempt *ttftAttemptTiming
	if len(timing.attempts) > 0 {
		attempt = timing.attempts[len(timing.attempts)-1]
	}
	if attempt == nil || !attempt.stages["stream_completed"].IsZero() ||
		(attempt.accountID != 0 && accountID != 0 && attempt.accountID != accountID) {
		attempt = timing.newAttemptLocked(accountID)
	}
	if attempt.accountID == 0 {
		attempt.accountID = accountID
	}
	attempt.stages["stream_started"] = time.Now()
	timing.mu.Unlock()
	return timing.attemptMarker(attempt)
}

// TTFTAttemptTimingSnapshots 返回深拷贝；日志与调用方修改不会污染在途采样。
func TTFTAttemptTimingSnapshots(c *gin.Context) []TTFTAttemptSnapshot {
	timing := ttftTimingFromContext(c)
	if timing == nil {
		return nil
	}
	timing.mu.Lock()
	defer timing.mu.Unlock()
	result := make([]TTFTAttemptSnapshot, 0, len(timing.attempts))
	for _, attempt := range timing.attempts {
		stages := make(map[string]int64, len(attempt.stages))
		for name, at := range attempt.stages {
			ms := at.Sub(timing.startedAt).Milliseconds()
			if ms < 0 {
				ms = 0
			}
			stages[name] = ms
		}
		result = append(result, TTFTAttemptSnapshot{Attempt: attempt.number, AccountID: attempt.accountID, StagesMS: stages, Transport: attempt.transport.Snapshot()})
	}
	return result
}

// TTFTRequestOperationSnapshot 包含请求级重复操作，与上游 attempt 分开，避免把排队算作推理。
func TTFTRequestOperationSnapshot(c *gin.Context) *latencytrace.Snapshot {
	timing := ttftTimingFromContext(c)
	if timing == nil {
		return nil
	}
	return timing.operations.Snapshot()
}

// withTTFTUpstreamTrace 将网络回调绑定当前 Do，保留原有 trace 和请求取消语义。
func withTTFTUpstreamTrace(c *gin.Context, req *http.Request, mark func(string)) *http.Request {
	timing := ttftTimingFromContext(c)
	if timing == nil || req == nil {
		return req
	}
	timing.mu.Lock()
	if len(timing.attempts) == 0 {
		timing.mu.Unlock()
		return req
	}
	recorder := timing.attempts[len(timing.attempts)-1].transport
	timing.mu.Unlock()
	ctx := latencytrace.WithRecorder(req.Context(), recorder)
	ctx = latencytrace.WithHTTPTrace(ctx, func() { mark("first_upstream_byte") })
	return req.WithContext(ctx)
}

func normalizeTTFTStageName(stage string) string {
	if len(stage) > 48 {
		stage = stage[:48]
	}
	// 正常调用点均为规范化常量，尤其 Flush 会重复标记，直接复用原字符串。
	normalized := true
	for i := 0; i < len(stage); i++ {
		ch := stage[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' {
			normalized = false
			break
		}
	}
	if normalized {
		return stage
	}
	// 异常名称仍按原规则裁剪和清洗；栈内缓冲避免逐次 append 扩容。
	var buf [48]byte
	b := buf[:0]
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
