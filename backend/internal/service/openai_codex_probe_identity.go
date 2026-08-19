package service

import (
	"net/http"
	"strings"
)

type codexProbePurpose string

const (
	codexProbePurposeUsageSnapshot      codexProbePurpose = "usage-snapshot"
	codexProbePurposeAccountTest        codexProbePurpose = "account-test"
	codexProbePurposeNativeCompactionV2 codexProbePurpose = "native-compaction-v2"
	codexProbePurposeImageAccountTest   codexProbePurpose = "image-account-test"
	codexProbePurposeQuotaOverdraft     codexProbePurpose = "quota-overdraft"
)

const codexProbeFingerprintSourceVersion = "codex-background-probe:v1"

// resolveCodexProbeFingerprintIDs 为后台额度与账号探测生成账号级身份。
// installation/session 继续复用账号持久化种子，thread/cache key 按探测用途和
// 模型稳定隔离，turn 每次请求重新生成，避免探测混入真实用户对话。
func resolveCodexProbeFingerprintIDs(account *Account, purpose codexProbePurpose, model string) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}

	purposeSeed := strings.ToLower(strings.TrimSpace(string(purpose)))
	modelSeed := strings.ToLower(strings.TrimSpace(model))
	source := codexProbeFingerprintSourceVersion + ":" + purposeSeed + ":" + modelSeed
	return resolveCodexFingerprintIDsWithSource(account, codexFingerprintSource{
		clientSessionID:      source,
		originalSessionID:    source,
		threadID:             source,
		promptCacheKey:       source,
		promptCacheKeyInBody: mode == codexFingerprintCockpit,
	}, mode)
}

// applyCodexProbeFingerprintHeaders 为没有入站 Codex 元数据的后台请求补齐头部
// 身份载体，再复用普通请求的统一改写逻辑。Header 与 Body 必须共享同一个 ids。
func applyCodexProbeFingerprintHeaders(headers http.Header, ids *codexFingerprintIDs) {
	if headers == nil || ids == nil {
		return
	}
	if strings.TrimSpace(headers.Get("x-codex-turn-metadata")) == "" {
		headers.Set("x-codex-turn-metadata", "{}")
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && strings.TrimSpace(headers.Get("conversation_id")) == "" {
		// applyCodexFingerprintHeaders 只重写已有 conversation_id；探测没有客户端头，
		// 先放入原始来源，使最终值与 Body prompt_cache_key 使用同一派生结果。
		headers.Set("conversation_id", ids.originalPromptCacheKey)
	}
	applyCodexFingerprintHeaders(headers, ids)
}
