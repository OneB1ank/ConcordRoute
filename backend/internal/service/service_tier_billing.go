package service

import (
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// ServiceTierBillingResolution 描述最终出站 tier、上游回显 tier 与实际计费 tier 的决策结果。
type ServiceTierBillingResolution struct {
	Requested  string
	Observed   string
	Billing    string
	Downgraded bool
}

// ResolveBillingServiceTier 只允许上游回显把费用降到更便宜的层级，禁止响应侧抬价。
func ResolveBillingServiceTier(requested, observed string) ServiceTierBillingResolution {
	requested = normalizeBillingServiceTier(requested)
	observed = normalizeBillingServiceTier(observed)
	resolution := ServiceTierBillingResolution{Requested: requested, Observed: observed, Billing: requested}
	if observed == "" || observed == requested {
		return resolution
	}
	observedRank, known := serviceTierCostRank(observed)
	if !known {
		return resolution
	}
	requestedRank, _ := serviceTierCostRank(requested)
	if observedRank >= requestedRank {
		return resolution
	}
	resolution.Billing = observed
	resolution.Downgraded = true
	return resolution
}

// serviceTierCostRank 按相对价格从低到高排列 tier；未知值不参与决策。
func serviceTierCostRank(tier string) (rank int, known bool) {
	switch normalizeBillingServiceTier(tier) {
	case "flex":
		return 0, true
	case "", "default", "standard", "auto", "scale":
		return 1, true
	case "priority", "fast":
		return 2, true
	default:
		return 1, false
	}
}

// ResolveOpenAIServiceTierBilling 按实际凭据协议决定是否采信回包 tier。
// ChatGPT Codex 私有端点会在有效 Fast 请求上回显 default，OAuth/Setup Token
// 因此保留最终出站 tier；公共 API Key 仍以上游回显的降级结果为准。
func ResolveOpenAIServiceTierBilling(account *Account, requested, observed string) ServiceTierBillingResolution {
	if isOpenAIOAuthLikeAccount(account) && codexOAuthResponseTierIsNonAuthoritative(observed) {
		return ServiceTierBillingResolution{
			Requested: normalizeBillingServiceTier(requested),
			Observed:  normalizeBillingServiceTier(observed),
			Billing:   normalizeBillingServiceTier(requested),
		}
	}
	return ResolveBillingServiceTier(requested, observed)
}

func isOpenAIOAuthLikeAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI &&
		(account.Type == AccountTypeOAuth || account.Type == AccountTypeSetupToken)
}

func codexOAuthResponseTierIsNonAuthoritative(observed string) bool {
	return normalizeBillingServiceTier(observed) == "default"
}

// ApplyOpenAIServiceTierBillingResolution 把最终计费 tier 写回结果，确保费用与用量日志一致。
func ApplyOpenAIServiceTierBillingResolution(account *Account, result *OpenAIForwardResult) ServiceTierBillingResolution {
	if result == nil {
		return ServiceTierBillingResolution{}
	}
	resolution := ResolveOpenAIServiceTierBilling(account, optionalStringValue(result.ServiceTier), result.UpstreamResponseServiceTier)
	if resolution.Downgraded {
		billing := resolution.Billing
		result.ServiceTier = &billing
	}
	return resolution
}

// logServiceTierBillingDowngrade 仅记录真实降档，避免正常 Fast 请求制造日志噪声。
func logServiceTierBillingDowngrade(account *Account, requestID string, resolution ServiceTierBillingResolution) {
	if !resolution.Downgraded {
		return
	}
	fields := []zap.Field{
		zap.String("request_id", strings.TrimSpace(requestID)),
		zap.String("requested_tier", resolution.Requested),
		zap.String("response_tier", resolution.Observed),
		zap.String("billed_tier", resolution.Billing),
	}
	if account != nil {
		fields = append(fields, zap.String("platform", account.Platform), zap.Int64("account_id", account.ID))
	}
	logger.L().Info("billing.service_tier_downgraded", fields...)
}

// observedOpenAIServiceTierFromPayload 从 Responses 或 Chat Completions 响应中提取回显 tier。
func observedOpenAIServiceTierFromPayload(payload []byte) string {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return ""
	}
	for _, path := range []string{"response.service_tier", "service_tier"} {
		value := gjson.GetBytes(payload, path)
		if !value.Exists() || value.Type != gjson.String {
			continue
		}
		if tier := normalizeObservedOpenAIServiceTier(value.String()); tier != "" {
			return tier
		}
	}
	return ""
}

func normalizeObservedOpenAIServiceTier(raw string) string {
	switch value := strings.ToLower(strings.TrimSpace(raw)); value {
	case "priority", "fast":
		return OpenAIFastTierPriority
	case "default", "flex", "scale":
		return value
	default:
		return ""
	}
}
