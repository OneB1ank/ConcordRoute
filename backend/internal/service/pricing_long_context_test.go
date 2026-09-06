package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/stretchr/testify/require"
)

// 复现远程目录只提供 above_272k 绝对价、没有 long_context_* 内部字段的真实格式。
const astraCatalogLongContextFixture = `{"gpt-6-astra":{
	"input_cost_per_token":0.00001,
	"input_cost_per_token_priority":0.00002,
	"input_cost_per_token_above_272k_tokens":0.00002,
	"output_cost_per_token":0.00005,
	"output_cost_per_token_priority":0.0001,
	"output_cost_per_token_above_272k_tokens":0.000075,
	"cache_creation_input_token_cost":0.0000125,
	"cache_creation_input_token_cost_priority":0.000025,
	"cache_read_input_token_cost":0.000001,
	"cache_read_input_token_cost_priority":0.000002,
	"supports_service_tier":true
}}`

func TestPricingCatalogAstraLongContextBilling(t *testing.T) {
	ps := &PricingService{}
	data, err := ps.parsePricingData([]byte(astraCatalogLongContextFixture))
	require.NoError(t, err)
	ps.pricingData = data
	bs := NewBillingService(&config.Config{}, ps)

	for _, tc := range []struct {
		name   string
		tokens UsageTokens
		tier   string
		want   float64
		long   bool
	}{
		{"at_threshold", UsageTokens{InputTokens: 272000, OutputTokens: 2000}, "", 2.82, false},
		{"above_threshold", UsageTokens{InputTokens: 272001, OutputTokens: 2000}, "", 5.59002, true},
		{"standard", UsageTokens{InputTokens: 300000, OutputTokens: 2000}, "", 6.15, true},
		{"priority", UsageTokens{InputTokens: 300000, OutputTokens: 2000}, "priority", 12.3, true},
		{"fast", UsageTokens{InputTokens: 300000, OutputTokens: 2000}, "fast", 12.3, true},
		{"flex", UsageTokens{InputTokens: 300000, OutputTokens: 2000}, "flex", 3.075, true},
		{"cached_threshold", UsageTokens{InputTokens: 200000, CacheReadTokens: 72000, OutputTokens: 2000}, "", 2.172, false},
		{"cached_long", UsageTokens{InputTokens: 200000, CacheReadTokens: 100000, OutputTokens: 2000}, "", 4.35, true},
		{"cache_write_long", UsageTokens{InputTokens: 200000, CacheCreationTokens: 100000, OutputTokens: 2000}, "", 6.65, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost, err := bs.CalculateCostWithServiceTier("gpt-6-astra", tc.tokens, 1, tc.tier)
			require.NoError(t, err)
			require.InDelta(t, tc.want, cost.TotalCost, 1e-10)
			require.InDelta(t, tc.want, cost.ActualCost, 1e-10)
			require.Equal(t, tc.long, cost.LongContextBillingApplied)
		})
	}
}

func TestPricingCatalogLongContextExplicitAndMalformedFields(t *testing.T) {
	// 显式内部字段优先，目录噪声、服务档后缀和缓存时长字段不应误变成上下文阶梯。
	for _, tc := range []struct {
		name          string
		fields        string
		threshold     int
		input, output float64
	}{
		{"explicit_disabled", `"long_context_input_token_threshold":0,"input_cost_per_token_above_272k_tokens":20`, 0, 0, 0},
		{"explicit_unit_multiplier", `"long_context_input_cost_multiplier":1,"input_cost_per_token_above_272k_tokens":20`, 0, 1, 0},
		{"explicit_policy", `"long_context_input_token_threshold":123,"long_context_input_cost_multiplier":3,"long_context_output_cost_multiplier":4,"input_cost_per_token_above_272k_tokens":20`, 123, 3, 4},
		{"explicit_null_is_absent", `"long_context_input_token_threshold":null,"input_cost_per_token_above_272k_tokens":20`, 272000, 2, 1},
		{"input_only", `"input_cost_per_token_above_272k_tokens":20`, 272000, 2, 1},
		{"output_only", `"output_cost_per_token_above_272k_tokens":75`, 272000, 1, 1.5},
		{"smallest_matching_threshold", `"input_cost_per_token_above_500k_tokens":40,"output_cost_per_token_above_500k_tokens":150,"input_cost_per_token_above_272k_tokens":20,"output_cost_per_token_above_272k_tokens":75`, 272000, 2, 1.5},
		{"do_not_mix_thresholds", `"input_cost_per_token_above_272k_tokens":20,"output_cost_per_token_above_500k_tokens":150`, 272000, 2, 1},
		{"tier_and_cache_fields_only", `"input_cost_per_token_above_272k_tokens_priority":40,"output_cost_per_token_above_272k_tokens_flex":37.5,"cache_creation_input_token_cost_above_1hr":20,"cache_read_input_token_cost_above_272k_tokens":2`, 0, 0, 0},
		{"invalid_prices", `"input_cost_per_token_above_272k_tokens":"20","output_cost_per_token_above_272k_tokens":-75`, 0, 0, 0},
		{"null_and_zero", `"input_cost_per_token_above_272k_tokens":null,"output_cost_per_token_above_272k_tokens":0`, 0, 0, 0},
		{"threshold_overflow", `"input_cost_per_token_above_9223372036854776k_tokens":20`, 0, 0, 0},
		{"invalid_threshold", `"input_cost_per_token_above_0k_tokens":20,"output_cost_per_token_above_-1k_tokens":75`, 0, 0, 0},
		{"no_surcharge", `"input_cost_per_token_above_272k_tokens":10,"output_cost_per_token_above_272k_tokens":50`, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"gpt-6-astra":{"input_cost_per_token":10,"output_cost_per_token":50,` + tc.fields + `}}`
			data, err := (&PricingService{}).parsePricingData([]byte(body))
			require.NoError(t, err)
			p := data["gpt-6-astra"]
			require.Equal(t, tc.threshold, p.LongContextInputTokenThreshold)
			require.InDelta(t, tc.input, p.LongContextInputCostMultiplier, 1e-12)
			require.InDelta(t, tc.output, p.LongContextOutputCostMultiplier, 1e-12)
		})
	}
}

func TestPricingCatalogAstraScopePreservesOtherModelPolicies(t *testing.T) {
	// 本次只修 Astra 缺失的动态阶梯，不顺带改变其他模型的既有计费合同。
	for _, model := range []string{"gpt-6-astra", "openai/gpt6astra-max", "gpt-6-astra-preview", "gpt-5.6-sol", "gemini-2.5-pro", "claude-sonnet-4", "gpt-6-astral"} {
		t.Run(model, func(t *testing.T) {
			var fixture map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(astraCatalogLongContextFixture), &fixture))
			body, err := json.Marshal(map[string]json.RawMessage{model: fixture["gpt-6-astra"]})
			require.NoError(t, err)
			data, err := (&PricingService{}).parsePricingData(body)
			require.NoError(t, err)
			want := 0
			if model == "gpt-6-astra" || model == "openai/gpt6astra-max" || model == "gpt-6-astra-preview" {
				want = 272000
			}
			require.Equal(t, want, data[model].LongContextInputTokenThreshold)
		})
	}
}

func TestPricingCatalogAstraLoadDoesNotLoseRemoteLadderToFallback(t *testing.T) {
	// 文件加载必须走实际解析和 fallback 合并链；内置同名条目不会修补远程条目的缺失字段。
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.json")
	fallback := filepath.Join(dir, "fallback.json")
	require.NoError(t, os.WriteFile(remote, []byte(astraCatalogLongContextFixture), 0600))
	require.NoError(t, os.WriteFile(fallback, []byte(`{"gpt-6-astra":{"input_cost_per_token":99}}`), 0600))
	ps := &PricingService{cfg: &config.Config{Pricing: config.PricingConfig{FallbackFile: fallback}}}
	require.NoError(t, ps.loadPricingData(remote))
	p := ps.GetModelPricing("gpt-6-astra")
	require.InDelta(t, 10e-6, p.InputCostPerToken, 1e-12)
	require.Equal(t, 272000, p.LongContextInputTokenThreshold)
	require.InDelta(t, 2, p.LongContextInputCostMultiplier, 1e-12)
	require.InDelta(t, 1.5, p.LongContextOutputCostMultiplier, 1e-12)
}

func TestPricingCatalogAstraDisplayAndSettlement(t *testing.T) {
	// 同一动态价卡要贯通公开价格、用量日志、余额/API Key 扣费命令及账号成本统计。
	ps := &PricingService{}
	data, err := ps.parsePricingData([]byte(astraCatalogLongContextFixture))
	require.NoError(t, err)
	ps.pricingData = data
	bs := NewBillingService(&config.Config{}, ps)
	display := bs.GetDisplayPricing("gpt-6-astra", 2, nil)
	require.Len(t, display.ContextIntervals, 2)
	long := display.ContextIntervals[1]
	require.InDelta(t, 40e-6, long.InputPricePerToken, 1e-12)
	require.InDelta(t, 150e-6, long.OutputPricePerToken, 1e-12)
	require.InDelta(t, 80e-6, long.FastInputPricePerToken, 1e-12)
	require.InDelta(t, 300e-6, long.FastOutputPricePerToken, 1e-12)

	for _, enabled := range []bool{true, false} {
		name := "enabled"
		want := 6.15
		if !enabled {
			name = "disabled"
			want = 3.1
		}
		t.Run(name, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
			svc.billingService = bs
			svc.resolver = NewModelPricingResolver(nil, bs)
			groupID := int64(1)
			err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
				APIKeyService: &openAIRecordUsageAPIKeyQuotaStub{},
				Result:        &OpenAIForwardResult{RequestID: "astra-catalog-" + name, Model: "gpt-6-astra", Usage: OpenAIUsage{InputTokens: 300000, OutputTokens: 2000}},
				APIKey:        &APIKey{ID: 1, GroupID: &groupID, Quota: 100, Group: &Group{ID: groupID, RateMultiplier: 2, LongContextPricingEnabled: enabled}},
				User:          &User{ID: 1},
				Account:       &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"quota_limit": 100}},
			})
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.Equal(t, enabled, usageRepo.lastLog.LongContextBillingApplied)
			require.InDelta(t, want, usageRepo.lastLog.TotalCost, 1e-10)
			require.InDelta(t, want*2, usageRepo.lastLog.ActualCost, 1e-10)
			billingRepo := requireOpenAIRecordUsageBillingRepoStub(t, svc)
			require.Equal(t, 1, billingRepo.calls)
			require.InDelta(t, want*2, billingRepo.lastCmd.BillableAmountUSD, 1e-10)
			require.InDelta(t, want*2, billingRepo.lastCmd.APIKeyQuotaCost, 1e-10)
			require.InDelta(t, want, billingRepo.lastCmd.AccountQuotaCost, 1e-10)
		})
	}
	accountCost := tryModelFilePricing(bs, "gpt-6-astra", UsageTokens{InputTokens: 300000, OutputTokens: 2000}, "priority")
	require.NotNil(t, accountCost)
	require.InDelta(t, 12.3, *accountCost, 1e-10)

	// 渠道显式区间是最终价格合同，即使目录有长上下文元数据也只收费一次。
	base, err := bs.GetModelPricing("gpt-6-astra")
	require.NoError(t, err)
	inputPrice, outputPrice := 7e-6, 13e-6
	cost, err := bs.CalculateCostUnified(CostInput{
		Model: "gpt-6-astra", Tokens: UsageTokens{InputTokens: 300000, OutputTokens: 2000}, RateMultiplier: 1,
		Resolver: NewModelPricingResolver(nil, bs),
		Resolved: &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceChannel, BasePricing: base,
			Intervals: []PricingInterval{{MinTokens: 0, InputPrice: &inputPrice, OutputPrice: &outputPrice}}},
	})
	require.NoError(t, err)
	require.InDelta(t, 2.126, cost.ActualCost, 1e-10)
	require.False(t, cost.LongContextBillingApplied)

	// 目录快照仍是原始单价，没有被展示或结算调用原地翻倍。
	encoded, err := json.Marshal(ps.pricingData["gpt-6-astra"])
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"input_cost_per_token":0.00001`)
}
