package service

import "testing"

func TestResolveOpenAIForwardModel(t *testing.T) {
	tests := []struct {
		name                        string
		account                     *Account
		requestedModel              string
		messagesDispatchMappedModel string
		expectedModel               string
	}{
		{
			name: "uses messages dispatch model for known claude family",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel:              "claude-opus-4-6",
			messagesDispatchMappedModel: "gpt-4o-mini",
			expectedModel:               "gpt-4o-mini",
		},
		{
			name: "uses exact messages dispatch model for unknown claude family",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel:              "claude-fable-5",
			messagesDispatchMappedModel: " gpt-5.6-sol ",
			expectedModel:               "gpt-5.6-sol",
		},
		{
			name:                        "nil account uses messages dispatch model",
			requestedModel:              "claude-fable-5",
			messagesDispatchMappedModel: "gpt-5.6-sol",
			expectedModel:               "gpt-5.6-sol",
		},
		{
			name:           "nil account without messages dispatch keeps requested model",
			requestedModel: "claude-fable-5",
			expectedModel:  "claude-fable-5",
		},
		{
			name: "ordinary unknown gpt model has no messages dispatch fallback",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt6",
			expectedModel:  "gpt6",
		},
		{
			name: "account exact mapping runs after messages dispatch model",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-5.6-sol": "gpt-5.5",
					},
				},
			},
			requestedModel:              "claude-fable-5",
			messagesDispatchMappedModel: "gpt-5.6-sol",
			expectedModel:               "gpt-5.5",
		},
		{
			name: "account wildcard mapping runs after messages dispatch model",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-*": "gpt-5.4",
					},
				},
			},
			requestedModel:              "claude-fable-5",
			messagesDispatchMappedModel: "gpt-5.6-sol",
			expectedModel:               "gpt-5.4",
		},
		{
			name: "account passthrough mapping runs after messages dispatch model",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-5.6-sol": "gpt-5.6-sol",
					},
				},
			},
			requestedModel:              "claude-fable-5",
			messagesDispatchMappedModel: "gpt-5.6-sol",
			expectedModel:               "gpt-5.6-sol",
		},
		{
			name: "account mapping cannot switch explicit Sol request to Luna",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-5.6-sol": "gpt-5.6-luna",
					},
				},
			},
			requestedModel: "gpt-5.6-sol",
			expectedModel:  "gpt-5.6-sol",
		},
		{
			name: "account mapping cannot switch explicit Luna request to Sol",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-5.6-luna": "gpt-5.6-sol",
					},
				},
			},
			requestedModel: "gpt-5.6-luna",
			expectedModel:  "gpt-5.6-luna",
		},
		{
			name: "explicit auto review alias may still target Luna",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"codex-auto-review": "gpt-5.6-luna",
					},
				},
			},
			requestedModel: "codex-auto-review",
			expectedModel:  "gpt-5.6-luna",
		},
		{
			name: "explicit Sol request may still target another generation",
			account: &Account{
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-5.6-sol": "gpt-5.5",
					},
				},
			},
			requestedModel: "gpt-5.6-sol",
			expectedModel:  "gpt-5.5",
		},
		{
			name: "ordinary codex spark request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt-5.3-codex-spark",
			expectedModel:  "gpt-5.3-codex-spark",
		},
		{
			name: "ordinary gpt-5.5 request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt-5.5",
			expectedModel:  "gpt-5.5",
		},
		{
			name: "ordinary gpt-5.5-pro request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt-5.5-pro",
			expectedModel:  "gpt-5.5-pro",
		},
		{
			name: "ordinary compact-spelled gpt5.5 request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt5.5",
			expectedModel:  "gpt5.5",
		},
		{
			name: "ordinary namespaced gpt-5.5 request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "openai/gpt-5.5",
			expectedModel:  "openai/gpt-5.5",
		},
		{
			name: "ordinary compact gpt-5.5 request keeps requested model",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel: "gpt-5.5-openai-compact",
			expectedModel:  "gpt-5.5-openai-compact",
		},
		{
			name: "whitespace-only messages dispatch model is ignored",
			account: &Account{
				Credentials: map[string]any{},
			},
			requestedModel:              "gpt-5.5",
			messagesDispatchMappedModel: "  ",
			expectedModel:               "gpt-5.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveOpenAIForwardModel(tt.account, tt.requestedModel, tt.messagesDispatchMappedModel); got != tt.expectedModel {
				t.Fatalf("resolveOpenAIForwardModel(...) = %q, want %q", got, tt.expectedModel)
			}
		})
	}
}

func TestOpenAIAccountModelMappingCompatibility(t *testing.T) {
	tests := []struct {
		name           string
		requestedModel string
		mappedModel    string
		wantCompatible bool
	}{
		{name: "Sol to Luna is rejected", requestedModel: "gpt-5.6-sol", mappedModel: "gpt-5.6-luna", wantCompatible: false},
		{name: "Luna to Terra is rejected", requestedModel: "gpt-5.6-luna", mappedModel: "gpt-5.6-terra", wantCompatible: false},
		{name: "generic GPT 5.6 remains Sol", requestedModel: "gpt-5.6", mappedModel: "gpt-5.6-sol", wantCompatible: true},
		{name: "review alias may target Luna", requestedModel: "codex-auto-review", mappedModel: "gpt-5.6-luna", wantCompatible: true},
		{name: "Sol to GPT 5.5 remains allowed", requestedModel: "gpt-5.6-sol", mappedModel: "gpt-5.5", wantCompatible: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpenAIAccountModelMappingCompatible(tt.requestedModel, tt.mappedModel); got != tt.wantCompatible {
				t.Fatalf("isOpenAIAccountModelMappingCompatible(%q, %q) = %v, want %v", tt.requestedModel, tt.mappedModel, got, tt.wantCompatible)
			}
		})
	}
}

func TestOpenAIAccountModelMappingRejectsCrossVariantScheduling(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-5.6-sol":       "gpt-5.6-luna",
				"codex-auto-review": "gpt-5.6-luna",
			},
		},
	}

	if account.IsModelSupported("gpt-5.6-sol") {
		t.Fatal("explicit gpt-5.6-sol must not select an account mapped to gpt-5.6-luna")
	}
	if !account.IsModelSupported("codex-auto-review") {
		t.Fatal("explicit codex-auto-review alias must retain its configured Luna mapping")
	}
}

func TestResolveOpenAICompactForwardModel(t *testing.T) {
	tests := []struct {
		name          string
		account       *Account
		model         string
		expectedModel string
	}{
		{
			name:          "nil account keeps original model",
			account:       nil,
			model:         "gpt-5.4",
			expectedModel: "gpt-5.4",
		},
		{
			name: "missing compact mapping keeps original model",
			account: &Account{
				Credentials: map[string]any{},
			},
			model:         "gpt-5.4",
			expectedModel: "gpt-5.4",
		},
		{
			name: "exact compact mapping overrides model",
			account: &Account{
				Credentials: map[string]any{
					"compact_model_mapping": map[string]any{
						"gpt-5.4": "gpt-5.4-openai-compact",
					},
				},
			},
			model:         "gpt-5.4",
			expectedModel: "gpt-5.4-openai-compact",
		},
		{
			name: "wildcard compact mapping overrides model",
			account: &Account{
				Credentials: map[string]any{
					"compact_model_mapping": map[string]any{
						"gpt-5.*": "gpt-5-openai-compact",
					},
				},
			},
			model:         "gpt-5.4",
			expectedModel: "gpt-5-openai-compact",
		},
		{
			name: "passthrough compact mapping remains unchanged",
			account: &Account{
				Credentials: map[string]any{
					"compact_model_mapping": map[string]any{
						"gpt-5.4": "gpt-5.4",
					},
				},
			},
			model:         "gpt-5.4",
			expectedModel: "gpt-5.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveOpenAICompactForwardModel(tt.account, tt.model); got != tt.expectedModel {
				t.Fatalf("resolveOpenAICompactForwardModel(...) = %q, want %q", got, tt.expectedModel)
			}
		})
	}
}

// TestResolveOpenAIAccountUpstreamModelForRequestMatchesForwardModes 验证限制检查与真实转发使用同一模型解析顺序。
func TestResolveOpenAIAccountUpstreamModelForRequestMatchesForwardModes(t *testing.T) {
	tests := []struct {
		name             string
		account          *Account
		model            string
		requireCompact   bool
		allowPassthrough bool
		want             string
	}{
		{
			name:    "OAuth 普通请求执行模型归一化",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "gpt-5.6",
			want:    "gpt-5.6-sol",
		},
		{
			name: "OAuth 账号映射后执行模型归一化",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"client-alias": "gpt-5.4-high"},
				},
			},
			model: "client-alias",
			want:  "gpt-5.4",
		},
		{
			name: "OAuth explicit Sol does not drift to Luna through account mapping",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gpt-5.6-sol": "gpt-5.6-luna"},
				},
			},
			model: "gpt-5.6-sol",
			want:  "gpt-5.6-sol",
		},
		{
			name: "compact 专属映射优先于 OAuth 归一化",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"compact_model_mapping": map[string]any{"gpt-5.6": "gpt-5.6-openai-compact"},
				},
			},
			model:          "gpt-5.6",
			requireCompact: true,
			want:           "gpt-5.6-openai-compact",
		},
		{
			name:           "compact 未映射时继续执行 OAuth 归一化",
			account:        &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:          "gpt-5.6",
			requireCompact: true,
			want:           "gpt-5.6-sol",
		},
		{
			name: "自动透传忽略普通账号映射",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{"openai_passthrough": true},
				Credentials: map[string]any{
					"model_mapping": map[string]any{"client-alias": "gpt-5.5"},
				},
			},
			model:            "client-alias",
			allowPassthrough: true,
			want:             "client-alias",
		},
		{
			name: "自动透传仍执行 compact 专属映射",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{"openai_passthrough": true},
				Credentials: map[string]any{
					"model_mapping":         map[string]any{"client-alias": "gpt-5.5"},
					"compact_model_mapping": map[string]any{"client-alias": "gpt-5.5-openai-compact"},
				},
			},
			model:            "client-alias",
			requireCompact:   true,
			allowPassthrough: true,
			want:             "gpt-5.5-openai-compact",
		},
		{
			name: "非 Responses 入口不套用自动透传规则",
			account: &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{"openai_passthrough": true},
				Credentials: map[string]any{
					"model_mapping": map[string]any{"client-alias": "gpt-5.4-high"},
				},
			},
			model: "client-alias",
			want:  "gpt-5.4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveOpenAIAccountUpstreamModelForRequest(tt.account, tt.model, tt.requireCompact, tt.allowPassthrough); got != tt.want {
				t.Fatalf("resolveOpenAIAccountUpstreamModelForRequest(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeCodexModel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.3-codex-spark":       "gpt-5.3-codex-spark",
		"gpt-5.3-codex-spark-high":  "gpt-5.3-codex-spark",
		"gpt-5.3-codex-spark-xhigh": "gpt-5.3-codex-spark",
		"gpt-5.3":                   "gpt-5.3-codex",
		"gpt-image-2":               "gpt-image-2",
		"gpt-5.4-nano":              "gpt-5.4-nano",
		"gpt-5.4-nano-high":         "gpt-5.4-nano",
		"gpt6":                      "gpt6",
		"claude-opus-4-6":           "claude-opus-4-6",
	}

	for input, expected := range cases {
		if got := normalizeCodexModel(input); got != expected {
			t.Fatalf("normalizeCodexModel(%q) = %q, want %q", input, got, expected)
		}
	}
}

func TestNormalizeOpenAIModelForUpstream(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		model   string
		want    string
	}{
		{
			name:    "nil account only trims whitespace",
			account: nil,
			model:   " gpt-5.6 ",
			want:    "gpt-5.6",
		},
		{
			name:    "oauth routes bare GPT-5.6 alias to Sol",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "gpt-5.6",
			want:    "gpt-5.6-sol",
		},
		{
			name:    "oauth routes provider-prefixed GPT-5.6 alias to Sol",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "openai/gpt-5.6",
			want:    "gpt-5.6-sol",
		},
		{
			name:    "oauth preserves unknown non codex model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "gemini-3-flash-preview",
			want:    "gemini-3-flash-preview",
		},
		{
			name:    "oauth preserves invalid gpt model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "gpt6",
			want:    "gpt6",
		},
		{
			name:    "oauth normalizes known codex alias",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "gpt-5.4-high",
			want:    "gpt-5.4",
		},
		{
			name:    "oauth preserves GPT-5.5 Pro model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "openai/gpt-5.5-pro",
			want:    "gpt-5.5-pro",
		},
		{
			name:    "oauth preserves codex auto review model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
			model:   "codex-auto-review",
			want:    "codex-auto-review",
		},
		{
			name:    "apikey preserves official bare GPT-5.6 alias",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			model:   "gpt-5.6",
			want:    "gpt-5.6",
		},
		{
			name:    "apikey preserves custom compatible model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			model:   "gemini-3-flash-preview",
			want:    "gemini-3-flash-preview",
		},
		{
			name:    "apikey preserves official non codex model",
			account: &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
			model:   "gpt-4.1",
			want:    "gpt-4.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeOpenAIModelForUpstream(tt.account, tt.model); got != tt.want {
				t.Fatalf("normalizeOpenAIModelForUpstream(...) = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUsageBillingModelCandidatesPreserveCodexAutoReviewModel(t *testing.T) {
	candidates := usageBillingModelCandidates("codex-auto-review")

	expected := []string{"codex-auto-review"}
	if len(candidates) != len(expected) {
		t.Fatalf("usageBillingModelCandidates(codex-auto-review) = %#v, want %#v", candidates, expected)
	}
	for i := range expected {
		if candidates[i] != expected[i] {
			t.Fatalf("usageBillingModelCandidates(codex-auto-review) = %#v, want %#v", candidates, expected)
		}
	}
}

func TestUsageBillingModelCandidatesPreserveGPT55ProModel(t *testing.T) {
	candidates := usageBillingModelCandidates("openai/gpt-5.5-pro")

	expected := []string{"openai/gpt-5.5-pro", "gpt-5.5-pro"}
	if len(candidates) != len(expected) {
		t.Fatalf("usageBillingModelCandidates(openai/gpt-5.5-pro) = %#v, want %#v", candidates, expected)
	}
	for i := range expected {
		if candidates[i] != expected[i] {
			t.Fatalf("usageBillingModelCandidates(openai/gpt-5.5-pro) = %#v, want %#v", candidates, expected)
		}
	}
}
