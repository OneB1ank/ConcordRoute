package service

import (
	"strconv"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
)

func lastOpenAIModelSegment(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = parts[len(parts)-1]
	}
	return strings.TrimSpace(model)
}

// canonicalizeOpenAIModelAliasSpelling 与提示词选择共用规范化规则，避免同一别名产生不同解释。
func canonicalizeOpenAIModelAliasSpelling(model string) string {
	return openai.CanonicalizeOpenAIModelAliasSpelling(model)
}

func openAIModelSupportsReasoningEffort(model string, effort string) bool {
	value := strings.ToLower(strings.TrimSpace(effort))
	if value == "" {
		return false
	}
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	switch value {
	case "max":
		return openAIModelSupportsMaxReasoningEffort(model)
	case "ultra":
		// Ultra 不是上游 reasoning effort，任何模型都不应声明支持。
		return false
	default:
		return true
	}
}

func openAIModelSupportsMaxReasoningEffort(model string) bool {
	return isOpenAIModelAtLeastVersion(model, 5, 6)
}

func isOpenAIModelAtLeastVersion(model string, minMajor, minMinor int) bool {
	major, minor, ok := parseOpenAIModelVersion(model)
	if !ok {
		return false
	}
	if major != minMajor {
		return major > minMajor
	}
	return minor >= minMinor
}

func parseOpenAIModelVersion(model string) (major int, minor int, ok bool) {
	normalized := canonicalizeOpenAIModelAliasSpelling(model)
	if normalized == "" || !strings.HasPrefix(normalized, "gpt-") {
		return 0, 0, false
	}

	rest := strings.TrimPrefix(normalized, "gpt-")
	majorEnd := 0
	for majorEnd < len(rest) && rest[majorEnd] >= '0' && rest[majorEnd] <= '9' {
		majorEnd++
	}
	if majorEnd == 0 {
		return 0, 0, false
	}

	major, err := strconv.Atoi(rest[:majorEnd])
	if err != nil {
		return 0, 0, false
	}

	minor = 0
	if majorEnd < len(rest) && rest[majorEnd] == '.' {
		minorStart := majorEnd + 1
		minorEnd := minorStart
		for minorEnd < len(rest) && rest[minorEnd] >= '0' && rest[minorEnd] <= '9' {
			minorEnd++
		}
		if minorEnd == minorStart {
			return 0, 0, false
		}
		minor, err = strconv.Atoi(rest[minorStart:minorEnd])
		if err != nil {
			return 0, 0, false
		}
	}

	return major, minor, true
}

func normalizeKnownOpenAICodexModel(model string) string {
	normalized := canonicalizeOpenAIModelAliasSpelling(model)
	if normalized == "" {
		return ""
	}

	if mapped := getNormalizedCodexModel(normalized); mapped != "" {
		return mapped
	}
	if strings.HasSuffix(normalized, "-openai-compact") {
		if mapped := getNormalizedCodexModel(strings.TrimSuffix(normalized, "-openai-compact")); mapped != "" {
			return mapped
		}
	}

	switch {
	case strings.HasPrefix(normalized, "gpt-live-"):
		// Live 的 codex 后缀不是文字模型别名，创建及 Sideband 必须保留语音模型。
		return normalized
	case normalized == "gpt-6-astra":
		return "gpt-6-astra"
	case strings.HasPrefix(normalized, "gpt-6-astra-"):
		suffix := strings.TrimPrefix(normalized, "gpt-6-astra-")
		if isKnownCodexModelSuffixForTarget("gpt-6-astra", suffix) {
			return "gpt-6-astra"
		}
		return ""
	case strings.Contains(normalized, "gpt-5.6-sol"):
		return "gpt-5.6-sol"
	case strings.Contains(normalized, "gpt-5.6-terra"):
		return "gpt-5.6-terra"
	case strings.Contains(normalized, "gpt-5.6-luna"):
		return "gpt-5.6-luna"
	case normalized == "gpt-5.6":
		return "gpt-5.6-sol"
	case strings.HasPrefix(normalized, "gpt-5.6-"):
		suffix := strings.TrimPrefix(normalized, "gpt-5.6-")
		if suffix == "max" || isKnownCodexModelSuffix(suffix) {
			return "gpt-5.6-sol"
		}
		return ""
	case strings.Contains(normalized, "gpt-5.5-pro"):
		return "gpt-5.5-pro"
	case strings.Contains(normalized, "gpt-5.5"):
		return "gpt-5.5"
	case strings.Contains(normalized, "gpt-5.4-mini"):
		return "gpt-5.4-mini"
	case strings.Contains(normalized, "gpt-5.4-nano"):
		return "gpt-5.4-nano"
	case strings.Contains(normalized, "gpt-5.4"):
		return "gpt-5.4"
	case strings.Contains(normalized, "gpt-5.2"):
		return "gpt-5.2"
	case strings.Contains(normalized, "gpt-5.3-codex-spark"):
		return "gpt-5.3-codex-spark"
	case strings.Contains(normalized, "gpt-5.3-codex"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "gpt-5.3"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "codex"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "gpt-5"):
		return "gpt-5.4"
	default:
		return ""
	}
}

// isOpenAIGPT56Model 判断是否 GPT-5.6 系列模型；入参可为原始模型名
// （含大小写/路径/后缀变体）或已归一化的基名，两者均能正确识别。
func isOpenAIGPT56Model(model string) bool {
	normalized := canonicalizeOpenAIModelAliasSpelling(model)
	if normalized == "gpt-5.6" {
		return true
	}
	if suffix, ok := strings.CutPrefix(normalized, "gpt-5.6-"); ok && (suffix == "max" || isKnownCodexModelSuffix(suffix)) {
		return true
	}
	for _, prefix := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"-") {
			return true
		}
	}
	return false
}

func appendUsageBillingModelCandidate(candidates []string, seen map[string]struct{}, model string) []string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return candidates
	}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}

	add(trimmed)
	if canonical := canonicalizeOpenAIModelAliasSpelling(trimmed); canonical != "" {
		add(canonical)
	}
	if normalized := normalizeKnownOpenAICodexModel(trimmed); normalized != "" {
		add(normalized)
	}
	return candidates
}

func usageBillingModelCandidates(primary string, alternates ...string) []string {
	seen := make(map[string]struct{}, 1+len(alternates))
	candidates := appendUsageBillingModelCandidate(nil, seen, primary)
	for _, alternate := range alternates {
		candidates = appendUsageBillingModelCandidate(candidates, seen, alternate)
	}
	return candidates
}

func firstUsageBillingModel(candidates []string) string {
	for _, candidate := range candidates {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
