package service

import (
	"net/http"
	"strings"
)

// UpstreamResponseObservation 是最终一次 HTTP 上游响应的非敏感摘要。
// 零值表示未采集；它不代表会话等级、模型能力或状态凭据有效性。
type UpstreamResponseObservation struct {
	StatusCode          int
	CodexTurnStateBytes int
}

// observeUpstreamResponse 只在最终结果构造时读取响应头一次，不读取流、不保留
// opaque state、不访问网络或数据库，也不改变现有重试和转发语义。
func observeUpstreamResponse(resp *http.Response) UpstreamResponseObservation {
	if resp == nil || resp.StatusCode < 100 || resp.StatusCode > 599 {
		return UpstreamResponseObservation{}
	}
	return UpstreamResponseObservation{
		StatusCode: resp.StatusCode,
		// 使用规范大小写常量，避免 Header.Get 为自定义头每次分配规范化字符串。
		CodexTurnStateBytes: len(strings.TrimSpace(resp.Header.Get("X-Codex-Turn-State"))),
	}
}

// applyUsageObservation 沿现有用量写入链路携带两个整数；WS 没有逐请求 HTTP
// 响应时保持未记录，尤其不把复用连接的握手头当作本回合新签发的状态。
func (o *UpstreamResponseObservation) applyUsageObservation(log *UsageLog) {
	if o == nil || log == nil || o.StatusCode == 0 {
		return
	}
	// worker 内复制为独立小快照，避免内嵌字段指针让用量队列保留整个转发结果和正文。
	snapshot := *o
	log.UpstreamStatusCode = &snapshot.StatusCode
	log.CodexTurnStateBytes = &snapshot.CodexTurnStateBytes
}
