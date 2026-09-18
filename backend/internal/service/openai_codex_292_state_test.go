package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// codex292ProxyAccountRepo 只实现实验状态机需要的窄代理查询能力。
type codex292ProxyAccountRepo struct {
	AccountRepository
	proxies map[int64]*Proxy
	errors  map[int64]error
	calls   []int64
}

func (r *codex292ProxyAccountRepo) GetCodex292ProxyByID(_ context.Context, id int64) (*Proxy, error) {
	r.calls = append(r.calls, id)
	if err := r.errors[id]; err != nil {
		return nil, err
	}
	proxy, ok := r.proxies[id]
	if !ok {
		return nil, ErrProxyNotFound
	}
	return proxy, nil
}

// codex292ValidationProxyRepo 复用完整接口的嵌入，仅记录配置校验实际查询的 ID。
type codex292ValidationProxyRepo struct {
	ProxyRepository
	proxies map[int64]*Proxy
	calls   []int64
}

// codex292AdminAccountRepo 只提供管理员 292 配置预校验需要的账号读取能力。
type codex292AdminAccountRepo struct {
	AccountRepository
	accounts    map[int64]*Account
	bulkUpdates int
	extraWrites int
}

func (r *codex292AdminAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	account, ok := r.accounts[id]
	if !ok {
		return nil, errors.New("account not found")
	}
	return account, nil
}

func (r *codex292AdminAccountRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	accounts := make([]*Account, 0, len(ids))
	for _, id := range ids {
		account, ok := r.accounts[id]
		if !ok {
			return nil, errors.New("account not found")
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func (r *codex292AdminAccountRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.extraWrites++
	return nil
}

func (r *codex292AdminAccountRepo) BulkUpdate(_ context.Context, _ []int64, _ AccountBulkUpdate) (int64, error) {
	r.bulkUpdates++
	return 0, nil
}

func (r *codex292ValidationProxyRepo) GetByID(_ context.Context, id int64) (*Proxy, error) {
	r.calls = append(r.calls, id)
	proxy, ok := r.proxies[id]
	if !ok {
		return nil, ErrProxyNotFound
	}
	return proxy, nil
}

func codex292TestProxy(id int64, host string) *Proxy {
	return &Proxy{
		ID:       id,
		Name:     host,
		Protocol: "socks5h",
		Host:     host,
		Port:     10808 + int(id),
		Status:   StatusActive,
	}
}

func codex292TestAccount() *Account {
	mainProxyID := int64(99)
	return &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		ProxyID:  &mainProxyID,
		Proxy:    codex292TestProxy(mainProxyID, "main-proxy.test"),
		Extra: map[string]any{
			Codex292StateInjectionEnabledExtraKey: true,
			Codex292StateAcquireProxyIDExtraKey:   float64(11),
			Codex292StateEgressProxyIDExtraKey:    float64(12),
		},
	}
}

func TestValidateCodex292StateConfig(t *testing.T) {
	repo := &codex292ValidationProxyRepo{proxies: map[int64]*Proxy{
		11: codex292TestProxy(11, "acquire.test"),
		12: codex292TestProxy(12, "egress.test"),
	}}

	require.NoError(t, validateCodex292StateConfig(context.Background(), repo, codex292TestAccount()))
	require.Equal(t, []int64{11}, repo.calls)

	disabled := codex292TestAccount()
	disabled.Extra[Codex292StateInjectionEnabledExtraKey] = false
	disabled.Platform = PlatformAnthropic
	require.NoError(t, validateCodex292StateConfig(context.Background(), nil, disabled))

	apiKey := codex292TestAccount()
	apiKey.Type = AccountTypeAPIKey
	require.ErrorContains(t, validateCodex292StateConfig(context.Background(), repo, apiKey), "requires a non-shadow OpenAI OAuth account")

	shadow := codex292TestAccount()
	parentID := int64(1)
	shadow.ParentAccountID = &parentID
	require.ErrorContains(t, validateCodex292StateConfig(context.Background(), repo, shadow), "requires a non-shadow OpenAI OAuth account")

	missing := codex292TestAccount()
	missing.Extra[Codex292StateEgressProxyIDExtraKey] = float64(404)
	require.NoError(t, validateCodex292StateConfig(context.Background(), repo, missing))

	negative := codex292TestAccount()
	negative.Extra[Codex292StateAcquireProxyIDExtraKey] = float64(-1)
	require.ErrorContains(t, validateCodex292StateConfig(context.Background(), repo, negative), "must be positive or empty")

	inactive := codex292TestAccount()
	repo.proxies[11].Status = StatusDisabled
	require.ErrorContains(t, validateCodex292StateConfig(context.Background(), repo, inactive), "inactive or expired")
	repo.proxies[11].Status = StatusActive

	expiredAt := time.Now().Add(-time.Second)
	repo.proxies[11].ExpiresAt = &expiredAt
	require.ErrorContains(t, validateCodex292StateConfig(context.Background(), repo, codex292TestAccount()), "inactive or expired")
	repo.proxies[11].ExpiresAt = nil
}

func TestAdminCodex292ExtraWritesUseUnifiedValidation(t *testing.T) {
	proxyRepo := &codex292ValidationProxyRepo{proxies: map[int64]*Proxy{
		11: codex292TestProxy(11, "acquire.test"),
		12: codex292TestProxy(12, "egress.test"),
	}}
	accountRepo := &codex292AdminAccountRepo{accounts: map[int64]*Account{
		7: codex292TestAccount(),
		8: {ID: 8, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{}},
	}}
	svc := &adminServiceImpl{accountRepo: accountRepo, proxyRepo: proxyRepo}

	err := svc.UpdateAccountExtra(context.Background(), 7, map[string]any{
		Codex292StateAcquireProxyIDExtraKey: float64(404),
	})
	require.ErrorContains(t, err, "does not exist")
	require.Zero(t, accountRepo.extraWrites)

	_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{8},
		Extra: map[string]any{
			Codex292StateInjectionEnabledExtraKey: true,
		},
	})
	require.ErrorContains(t, err, "requires a non-shadow OpenAI OAuth account")
	require.Zero(t, accountRepo.bulkUpdates)
}

func TestPrepareOpenAICodex292RequestDisabledPreservesLegacyProxyAndState(t *testing.T) {
	account := codex292TestAccount()
	account.Extra[Codex292StateInjectionEnabledExtraKey] = false
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set(openAICodexTurnStateHeader, "client-state")

	plan, proxyURL, err := (&OpenAIGatewayService{}).prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", req)

	require.NoError(t, err)
	require.False(t, plan.enabled)
	require.Equal(t, account.Proxy.URL(), proxyURL)
	require.Equal(t, "client-state", req.Header.Get(openAICodexTurnStateHeader))
}

func TestOpenAICodex292StateLifecycleUsesAcquireProxyAndAccountProxy(t *testing.T) {
	repo := &codex292ProxyAccountRepo{proxies: map[int64]*Proxy{
		11: codex292TestProxy(11, "acquire.test"),
		12: codex292TestProxy(12, "egress.test"),
	}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := codex292TestAccount()
	fullState := strings.Repeat("f", openAICodexTeamFullStateLength)
	degradedState := strings.Repeat("d", openAICodexTeamDegradedStateLength)

	acquireReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	acquireReq.Header.Set(openAICodexTurnStateHeader, "unknown-client-state")
	acquirePlan, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "GPT-5.4", acquireReq)
	require.NoError(t, err)
	require.True(t, acquirePlan.enabled)
	require.False(t, acquirePlan.usedState)
	require.Equal(t, repo.proxies[11].URL(), proxyURL)
	require.Equal(t, "unknown-client-state", acquireReq.Header.Get(openAICodexTurnStateHeader))
	require.NotEqual(t, account.Proxy.URL(), proxyURL)

	svc.observeOpenAICodex292Response(acquirePlan, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{fullState}},
	})
	// 旧版出口代理配置即使变化，也不应使账号主代理业务缓存失效。
	account.Extra[Codex292StateEgressProxyIDExtraKey] = float64(404)

	egressReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	egressReq.Header.Set(openAICodexTurnStateHeader, "stale-client-state")
	egressPlan, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", egressReq)
	require.NoError(t, err)
	require.True(t, egressPlan.usedState)
	require.Equal(t, account.Proxy.URL(), proxyURL)
	require.Equal(t, fullState, egressReq.Header.Get(openAICodexTurnStateHeader))

	otherModelReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	otherModelPlan, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.5", otherModelReq)
	require.NoError(t, err)
	require.False(t, otherModelPlan.usedState)
	require.Equal(t, repo.proxies[11].URL(), proxyURL)
	require.Empty(t, otherModelReq.Header.Get(openAICodexTurnStateHeader))

	svc.observeOpenAICodex292Response(egressPlan, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{degradedState}},
	})
	reacquireReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reacquirePlan, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", reacquireReq)
	require.NoError(t, err)
	require.False(t, reacquirePlan.usedState)
	require.Equal(t, repo.proxies[11].URL(), proxyURL)
	require.Empty(t, reacquireReq.Header.Get(openAICodexTurnStateHeader))

	require.NotContains(t, account.Extra, openAICodexTurnStateHeader)
	require.NotContains(t, account.Credentials, fullState)
}

func TestOpenAICodex292StateExpiresAndIgnoresUnknownLengths(t *testing.T) {
	repo := &codex292ProxyAccountRepo{proxies: map[int64]*Proxy{
		11: codex292TestProxy(11, "acquire.test"),
		12: codex292TestProxy(12, "egress.test"),
	}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := codex292TestAccount()
	key := openAICodex292StateKey{accountID: account.ID, model: "gpt-5.4"}
	svc.openaiCodex292States.Store(key, openAICodex292StateEntry{
		value:          "expired-state",
		expiresAt:      time.Now().Add(-time.Second),
		acquireProxyID: 11,
		egressProxyID:  12,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	plan, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", req)
	require.NoError(t, err)
	require.False(t, plan.usedState)
	require.Equal(t, repo.proxies[11].URL(), proxyURL)

	clientStateReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	clientStateReq.Header.Set(openAICodexTurnStateHeader, "client-state-after-expiry")
	plan, proxyURL, err = svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", clientStateReq)
	require.NoError(t, err)
	require.False(t, plan.usedState)
	require.Equal(t, repo.proxies[11].URL(), proxyURL)
	require.Equal(t, "client-state-after-expiry", clientStateReq.Header.Get(openAICodexTurnStateHeader))

	svc.observeOpenAICodex292Response(plan, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{strings.Repeat("u", openAICodexTeamFullStateLength+1)}},
	})
	_, exists := svc.openaiCodex292States.Load(key)
	require.False(t, exists)

	fullState := strings.Repeat("f", openAICodexTeamFullStateLength)
	svc.observeOpenAICodex292Response(plan, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{fullState}},
	})
	svc.observeOpenAICodex292Response(plan, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{http.CanonicalHeaderKey(openAICodexTurnStateHeader): []string{strings.Repeat("u", openAICodexTeamFullStateLength+1)}},
	})
	// 旧数字即使出现在 HTTP 状态码位置，也不得被当作撤销信号。
	svc.observeOpenAICodex292Response(plan, &http.Response{StatusCode: openAICodexProDegradedStateLength})
	raw, exists := svc.openaiCodex292States.Load(key)
	require.True(t, exists)
	entry, ok := raw.(openAICodex292StateEntry)
	require.True(t, ok)
	require.Equal(t, fullState, entry.value)
}

func TestOpenAICodex292StateClassifiesProAndTeamLengths(t *testing.T) {
	for _, tc := range []struct {
		name           string
		fullLength     int
		degradedLength int
	}{
		{name: "pro", fullLength: openAICodexProFullStateLength, degradedLength: openAICodexProDegradedStateLength},
		{name: "team", fullLength: openAICodexTeamFullStateLength, degradedLength: openAICodexTeamDegradedStateLength},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			plan := openAICodex292RequestPlan{
				enabled: true,
				key:     openAICodex292StateKey{accountID: 7, model: "gpt-5.4"},
			}
			fullState := strings.Repeat("f", tc.fullLength)
			svc.observeOpenAICodex292Response(plan, &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{openAICodexTurnStateCanonicalHeader: []string{fullState}},
			})
			raw, exists := svc.openaiCodex292States.Load(plan.key)
			require.True(t, exists)
			entry, ok := raw.(openAICodex292StateEntry)
			require.True(t, ok)
			require.Equal(t, fullState, entry.value)

			svc.observeOpenAICodex292Response(plan, &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{openAICodexTurnStateCanonicalHeader: []string{strings.Repeat("d", tc.degradedLength)}},
			})
			_, exists = svc.openaiCodex292States.Load(plan.key)
			require.False(t, exists)
		})
	}
}

func TestOpenAICodex292ProxyFailureDoesNotFallBackToMainProxy(t *testing.T) {
	expiredAt := time.Now().Add(-time.Minute)
	repo := &codex292ProxyAccountRepo{proxies: map[int64]*Proxy{
		11: {
			ID:        11,
			Protocol:  "socks5h",
			Host:      "expired.test",
			Port:      10808,
			Status:    StatusActive,
			ExpiresAt: &expiredAt,
		},
	}, errors: map[int64]error{}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := codex292TestAccount()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	_, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", req)
	require.ErrorContains(t, err, "inactive or expired")
	require.Empty(t, proxyURL)

	repo.proxies = nil
	repo.errors[11] = errors.New("proxy storage unavailable")
	_, proxyURL, err = svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", req)
	require.ErrorContains(t, err, "proxy storage unavailable")
	require.Empty(t, proxyURL)
}

func TestOpenAICodex292ProxyCacheDoesNotOutliveProxyExpiry(t *testing.T) {
	expiresAt := time.Now().Add(200 * time.Millisecond)
	proxy := codex292TestProxy(11, "short-lived.test")
	proxy.ExpiresAt = &expiresAt
	repo := &codex292ProxyAccountRepo{proxies: map[int64]*Proxy{11: proxy}, errors: map[int64]error{}}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := codex292TestAccount()
	account.Extra[Codex292StateEgressProxyIDExtraKey] = float64(0)

	_, proxyURL, err := svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.NoError(t, err)
	require.Equal(t, proxy.URL(), proxyURL)
	raw, ok := svc.openaiCodex292ProxyURLs.Load(int64(11))
	require.True(t, ok)
	entry, ok := raw.(openAICodex292ProxyCacheEntry)
	require.True(t, ok)
	require.Equal(t, expiresAt, entry.expiresAt)

	time.Sleep(time.Until(expiresAt) + 20*time.Millisecond)
	_, proxyURL, err = svc.prepareOpenAICodex292Request(context.Background(), account, "gpt-5.4", httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	require.ErrorContains(t, err, "inactive or expired")
	require.Empty(t, proxyURL)
	require.Equal(t, []int64{11, 11}, repo.calls)
}

func TestSendCCUpstreamRequestUsesCodex292ProxyLifecycle(t *testing.T) {
	repo := &codex292ProxyAccountRepo{proxies: map[int64]*Proxy{
		11: codex292TestProxy(11, "acquire.test"),
		12: codex292TestProxy(12, "egress.test"),
	}}
	fullState := strings.Repeat("c", openAICodexProFullStateLength)
	issuedHeader := make(http.Header)
	issuedHeader.Set(openAICodexTurnStateHeader, fullState)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: issuedHeader, Body: io.NopCloser(strings.NewReader(`{}`))},
		{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))},
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	account := codex292TestAccount()
	c, _ := newTurnStateTestContext(t, 1, "cc-session")

	resp, err := svc.sendCCUpstreamRequest(
		context.Background(), c, account, "https://upstream.test/v1/chat/completions",
		[]byte(`{"model":"gpt-5.4","messages":[]}`), "gpt-5.4", false, "token", "", "",
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, repo.proxies[11].URL(), upstream.lastProxyURL)
	require.Empty(t, upstream.lastReq.Header.Get(openAICodexTurnStateHeader))
	require.NoError(t, resp.Body.Close())

	resp, err = svc.sendCCUpstreamRequest(
		context.Background(), c, account, "https://upstream.test/v1/chat/completions",
		[]byte(`{"model":"gpt-5.4","messages":[]}`), "gpt-5.4", false, "token", "", "",
	)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, account.Proxy.URL(), upstream.lastProxyURL)
	require.Equal(t, fullState, upstream.lastReq.Header.Get(openAICodexTurnStateHeader))
	require.NoError(t, resp.Body.Close())
}
