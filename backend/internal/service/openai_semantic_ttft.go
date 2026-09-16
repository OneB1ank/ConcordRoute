package service

import (
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
)

// 展示口径只观察首个语义事件；真实首内容和转发状态机继续由原逻辑维护。
func recordOpenAISemanticFirstTokenMs(first **int, started time.Time, payload []byte, eventType string) {
	if first != nil && *first == nil && openai.StreamDataStartsSemanticOutputBytes(payload, eventType) {
		recordFirstTokenMs(first, started)
	}
}

// usageFirstTokenMs 只在入库边界选择展示值，不回写调度器读取的 FirstTokenMs。
// 没有语义样本的旧调用方、其它平台与非流式路径继续沿用原首内容值。
func (r *OpenAIForwardResult) usageFirstTokenMs(account *Account) *int {
	if r == nil {
		return nil
	}
	if account != nil && account.Platform == PlatformOpenAI &&
		(r.Stream || r.OpenAIWSMode) && r.SemanticFirstTokenMs != nil {
		return r.SemanticFirstTokenMs
	}
	return r.FirstTokenMs
}
