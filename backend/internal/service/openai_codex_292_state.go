package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/errors"
)

const (
	// Codex292StateInjectionEnabledExtraKey 控制账号级实验性 Turn-State 注入。
	// 键名保留 292 是为了兼容已保存的账号配置，协议判据使用当前状态值长度。
	Codex292StateInjectionEnabledExtraKey = "codex_292_state_injection_enabled"
	// Codex292StateAcquireProxyIDExtraKey 指定没有可用满血 state 时的采集出口代理。
	Codex292StateAcquireProxyIDExtraKey = "codex_292_state_acquire_proxy_id"
	// Codex292StateEgressProxyIDExtraKey 指定持有 state 后业务请求使用的出口代理。
	Codex292StateEgressProxyIDExtraKey = "codex_292_state_egress_proxy_id"

	// Turn-State 是响应头中的 opaque ASCII 字符串；HTTP 响应通常仍为 200。
	// Pro 与 Team 使用不同长度，四个值都不是 HTTP 状态码。
	openAICodexProFullStateLength      = 292
	openAICodexProDegradedStateLength  = 312
	openAICodexTeamFullStateLength     = 332
	openAICodexTeamDegradedStateLength = 356
	// 外部观察没有稳定的服务端 TTL 契约，因此使用保守租约并由下一笔业务请求惰性续取。
	openAICodex292StateLease    = 55 * time.Minute
	openAICodex292ProxyCacheTTL = 30 * time.Second
)

// Codex292ProxyLookup 是网关按账号扩展配置解析辅助代理的窄仓储接口。
// 它刻意不扩展 AccountRepository，避免只读调用方被迫依赖代理管理能力。
type Codex292ProxyLookup interface {
	GetCodex292ProxyByID(ctx context.Context, id int64) (*Proxy, error)
}

func (a *Account) IsCodex292StateInjectionEnabled() bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeOAuth &&
		!a.IsShadow() && resolveAccountExtraBool(a.Extra, Codex292StateInjectionEnabledExtraKey)
}

func (a *Account) GetCodex292StateAcquireProxyID() int64 {
	if a == nil {
		return 0
	}
	return int64(a.getExtraInt(Codex292StateAcquireProxyIDExtraKey))
}

func (a *Account) GetCodex292StateEgressProxyID() int64 {
	if a == nil {
		return 0
	}
	return int64(a.getExtraInt(Codex292StateEgressProxyIDExtraKey))
}

// validateCodex292StateConfig 在账号写入前验证实验开关与辅助代理引用。
// 两个代理都允许留空，表示对应阶段直连；账号通用 proxy_id 不作为隐式回退。
func validateCodex292StateConfig(ctx context.Context, proxyRepo ProxyRepository, account *Account) error {
	if account == nil || !resolveAccountExtraBool(account.Extra, Codex292StateInjectionEnabledExtraKey) {
		return nil
	}
	if account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth || account.IsShadow() {
		return infraerrors.BadRequest(
			"CODEX_292_STATE_ACCOUNT_UNSUPPORTED",
			"Codex turn-state injection requires a non-shadow OpenAI OAuth account",
		)
	}
	ids := []int64{account.GetCodex292StateAcquireProxyID(), account.GetCodex292StateEgressProxyID()}
	seen := make(map[int64]struct{}, len(ids))
	for _, proxyID := range ids {
		if proxyID < 0 {
			return infraerrors.BadRequest("CODEX_292_PROXY_INVALID", "Codex turn-state proxy ID must be positive or empty")
		}
		if proxyID == 0 {
			continue
		}
		if _, ok := seen[proxyID]; ok {
			continue
		}
		seen[proxyID] = struct{}{}
		if proxyRepo == nil {
			return infraerrors.BadRequest("CODEX_292_PROXY_UNAVAILABLE", "proxy repository is unavailable")
		}
		proxy, err := proxyRepo.GetByID(ctx, proxyID)
		if err != nil {
			return infraerrors.Newf(http.StatusBadRequest, "CODEX_292_PROXY_INVALID", "Codex turn-state proxy %d does not exist", proxyID)
		}
		if proxy == nil || !proxy.IsActive() || proxy.IsExpired(time.Now()) {
			return infraerrors.Newf(http.StatusBadRequest, "CODEX_292_PROXY_INACTIVE", "Codex turn-state proxy %d is inactive or expired", proxyID)
		}
	}
	return nil
}

func codex292StateConfigUpdatesProvided(updates map[string]any) bool {
	for _, key := range []string{
		Codex292StateInjectionEnabledExtraKey,
		Codex292StateAcquireProxyIDExtraKey,
		Codex292StateEgressProxyIDExtraKey,
	} {
		if _, ok := updates[key]; ok {
			return true
		}
	}
	return false
}

// accountWithCodex292ExtraUpdates 构造只用于配置校验的账号副本，避免校验失败时
// 修改仓储返回对象，也避免把整个 Extra 覆盖写回数据库。
func accountWithCodex292ExtraUpdates(account *Account, updates map[string]any) *Account {
	if account == nil {
		return nil
	}
	prospective := *account
	prospective.Extra = make(map[string]any, len(account.Extra)+len(updates))
	for key, value := range account.Extra {
		prospective.Extra[key] = value
	}
	for key, value := range updates {
		prospective.Extra[key] = value
	}
	return &prospective
}

type openAICodex292StateKey struct {
	accountID int64
	model     string
}

type openAICodex292StateEntry struct {
	value          string
	expiresAt      time.Time
	acquireProxyID int64
	egressProxyID  int64
}

type openAICodex292ProxyCacheEntry struct {
	url       string
	expiresAt time.Time
}

type openAICodex292RequestPlan struct {
	enabled        bool
	key            openAICodex292StateKey
	acquireProxyID int64
	egressProxyID  int64
	usedState      bool
}

func normalizeOpenAICodex292Model(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return "*"
	}
	return model
}

// prepareOpenAICodex292Request 在开关关闭时完整保留原账号代理与客户端回带头。
// 开关开启后，账号通用 proxy_id 不参与模型请求：无 state 走采集代理，有 state
// 走业务出口代理，并由服务端状态覆盖客户端值。
func (s *OpenAIGatewayService) prepareOpenAICodex292Request(
	ctx context.Context,
	account *Account,
	model string,
	req *http.Request,
) (openAICodex292RequestPlan, string, error) {
	if account == nil || req == nil || !account.IsCodex292StateInjectionEnabled() {
		return openAICodex292RequestPlan{}, resolveAccountProxyURL(account), nil
	}

	plan := openAICodex292RequestPlan{
		enabled:        true,
		key:            openAICodex292StateKey{accountID: account.ID, model: normalizeOpenAICodex292Model(model)},
		acquireProxyID: account.GetCodex292StateAcquireProxyID(),
		egressProxyID:  account.GetCodex292StateEgressProxyID(),
	}

	now := time.Now()
	if raw, ok := s.openaiCodex292States.Load(plan.key); ok {
		entry, valid := raw.(openAICodex292StateEntry)
		if valid && entry.value != "" && now.Before(entry.expiresAt) &&
			entry.acquireProxyID == plan.acquireProxyID && entry.egressProxyID == plan.egressProxyID {
			req.Header.Set(openAICodexTurnStateHeader, entry.value)
			plan.usedState = true
		} else {
			s.openaiCodex292States.Delete(plan.key)
		}
	}
	if !plan.usedState {
		// 采集阶段不沿用客户端或旧实例带来的 state，避免把未知来源误判为本账号租约。
		req.Header.Del(openAICodexTurnStateHeader)
	}

	proxyID := plan.acquireProxyID
	if plan.usedState {
		proxyID = plan.egressProxyID
	}
	proxyURL, err := s.resolveOpenAICodex292ProxyURL(ctx, proxyID)
	if err != nil {
		return openAICodex292RequestPlan{}, "", err
	}
	return plan, proxyURL, nil
}

func (s *OpenAIGatewayService) resolveOpenAICodex292ProxyURL(ctx context.Context, proxyID int64) (string, error) {
	if proxyID <= 0 {
		return "", nil
	}
	if raw, ok := s.openaiCodex292ProxyURLs.Load(proxyID); ok {
		entry, valid := raw.(openAICodex292ProxyCacheEntry)
		if valid && time.Now().Before(entry.expiresAt) {
			return entry.url, nil
		}
		s.openaiCodex292ProxyURLs.Delete(proxyID)
	}
	lookup, ok := s.accountRepo.(Codex292ProxyLookup)
	if !ok {
		return "", fmt.Errorf("codex turn-state proxy lookup is unavailable")
	}
	proxy, err := lookup.GetCodex292ProxyByID(ctx, proxyID)
	if err != nil {
		return "", fmt.Errorf("resolve codex turn-state proxy %d: %w", proxyID, err)
	}
	if proxy == nil || !proxy.IsActive() || proxy.IsExpired(time.Now()) {
		return "", fmt.Errorf("codex turn-state proxy %d is inactive or expired", proxyID)
	}
	proxyURL := strings.TrimSpace(proxy.URL())
	if proxyURL == "" {
		return "", fmt.Errorf("codex turn-state proxy %d has an empty URL", proxyID)
	}
	cacheExpiresAt := time.Now().Add(openAICodex292ProxyCacheTTL)
	if proxy.ExpiresAt != nil && proxy.ExpiresAt.Before(cacheExpiresAt) {
		cacheExpiresAt = *proxy.ExpiresAt
	}
	s.openaiCodex292ProxyURLs.Store(proxyID, openAICodex292ProxyCacheEntry{
		url:       proxyURL,
		expiresAt: cacheExpiresAt,
	})
	return proxyURL, nil
}

func isOpenAICodexFullStateLength(length int) bool {
	return length == openAICodexProFullStateLength || length == openAICodexTeamFullStateLength
}

func isOpenAICodexDegradedStateLength(length int) bool {
	return length == openAICodexProDegradedStateLength || length == openAICodexTeamDegradedStateLength
}

// observeOpenAICodex292Response 只消费最终 HTTP 响应头。Pro 292、Team 332
// 写入短期租约，Pro 312、Team 356 立即撤销；HTTP 状态码不参与判定。
func (s *OpenAIGatewayService) observeOpenAICodex292Response(plan openAICodex292RequestPlan, resp *http.Response) {
	if s == nil || !plan.enabled || resp == nil {
		return
	}
	state := extractOpenAICodexTurnState(resp.Header)
	if isOpenAICodexDegradedStateLength(len(state)) {
		s.openaiCodex292States.Delete(plan.key)
		return
	}
	if isOpenAICodexFullStateLength(len(state)) {
		s.openaiCodex292States.Store(plan.key, openAICodex292StateEntry{
			value:          state,
			expiresAt:      time.Now().Add(openAICodex292StateLease),
			acquireProxyID: plan.acquireProxyID,
			egressProxyID:  plan.egressProxyID,
		})
	}
}

func (s *OpenAIGatewayService) invalidateOpenAICodex292State(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openaiCodex292States.Range(func(key, _ any) bool {
		stateKey, ok := key.(openAICodex292StateKey)
		if !ok || stateKey.accountID == accountID {
			s.openaiCodex292States.Delete(key)
		}
		return true
	})
}

func (s *OpenAIGatewayService) invalidateOpenAICodex292Proxy(proxyID int64) {
	if s == nil || proxyID <= 0 {
		return
	}
	s.openaiCodex292ProxyURLs.Delete(proxyID)
}
