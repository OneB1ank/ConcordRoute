package service

// CodexTurnStateRequestMode 描述网关对出站请求侧 Turn-State 头采取的动作。
// 该值是受控的小枚举，不包含 opaque state 原文或代理 URL。
const (
	CodexTurnStateRequestModeInjected    = "injected"
	CodexTurnStateRequestModeAcquire     = "acquire"
	CodexTurnStateRequestModeDisabled    = "disabled"
	CodexTurnStateRequestModeNotRecorded = "not_recorded"
)

// codexTurnStateRequestModeForPlan 将内存中的请求计划转换为受支持 OpenAI
// 请求的持久化观测值。其它平台和历史路径保持 nil，便于界面区别“未接入”与“已关闭”。
func codexTurnStateRequestModeForPlan(account *Account, plan openAICodex292RequestPlan) *string {
	if account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	mode := CodexTurnStateRequestModeDisabled
	if plan.enabled {
		mode = CodexTurnStateRequestModeAcquire
		if plan.usedState {
			mode = CodexTurnStateRequestModeInjected
		}
	}
	return &mode
}

func applyCodexTurnStateRequestMode(result *OpenAIForwardResult, account *Account, plan openAICodex292RequestPlan) {
	if result == nil {
		return
	}
	result.CodexTurnStateRequestMode = codexTurnStateRequestModeForPlan(account, plan)
}
