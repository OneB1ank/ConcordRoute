package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestOAuthAccount(id int64, extra map[string]any) *Account {
	account := &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
	EnsureCodexFingerprintSeed(account)
	return account
}

// --- deriveStableUUIDv4 ---

func TestDeriveStableUUIDv4_Deterministic(t *testing.T) {
	a := deriveStableUUIDv4("test-seed-1")
	b := deriveStableUUIDv4("test-seed-1")
	assert.Equal(t, a, b, "同一种子应返回相同结果")
}

func TestDeriveStableUUIDv4_DifferentSeeds(t *testing.T) {
	a := deriveStableUUIDv4("seed-a")
	b := deriveStableUUIDv4("seed-b")
	assert.NotEqual(t, a, b, "不同种子应返回不同结果")
}

func TestDeriveStableUUIDv4_ValidFormat(t *testing.T) {
	result := deriveStableUUIDv4("test-seed")
	parsed, err := uuid.Parse(result)
	require.NoError(t, err, "应返回合法 UUID 格式")
	assert.Equal(t, uuid.Version(4), parsed.Version(), "应为 UUIDv4")
	assert.Equal(t, uuid.RFC4122, parsed.Variant(), "应为 RFC4122 变体")
}

func TestCodexFingerprintSeed_ShadowUsesParentSeed(t *testing.T) {
	parentID := int64(42)
	parent := newTestOAuthAccount(parentID, nil)
	shadow := newTestOAuthAccount(99, map[string]any{
		CodexFingerprintSeedExtraKey: parent.GetExtraString(CodexFingerprintSeedExtraKey),
	})
	shadow.ParentAccountID = &parentID

	assert.Equal(t, resolveConvergedInstallationID(parent), resolveConvergedInstallationID(shadow))
	assert.Equal(t, resolveConvergedSessionID(parent), resolveConvergedSessionID(shadow))
	assert.Equal(t, resolveConvergedThreadID(parent, "client-session"), resolveConvergedThreadID(shadow, "client-session"))
}

func TestEnsureCodexFingerprintSeed_StableAndUnique(t *testing.T) {
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	b := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	seedA := EnsureCodexFingerprintSeed(a)
	seedAAgain := EnsureCodexFingerprintSeed(a)
	seedB := EnsureCodexFingerprintSeed(b)

	require.NotEmpty(t, seedA)
	require.NoError(t, uuid.Validate(seedA))
	assert.Equal(t, seedA, seedAAgain, "同一账号对象不得重复轮换种子")
	assert.NotEqual(t, seedA, seedB, "相同本地账号 ID 的独立记录必须获得不同随机种子")
}

func TestEnsureCodexFingerprintSeed_NormalizesSupportedUUIDForms(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			CodexFingerprintSeedExtraKey: "urn:uuid:11111111-1111-4111-8111-111111111111",
		},
	}

	seed := EnsureCodexFingerprintSeed(account)

	require.Equal(t, "11111111-1111-4111-8111-111111111111", seed)
	require.Equal(t, seed, account.Extra[CodexFingerprintSeedExtraKey])
}

func TestEnsureCodexFingerprintSeed_ReplacesInvalidAndNilUUID(t *testing.T) {
	for _, invalid := range []string{"broken", uuid.Nil.String()} {
		t.Run(invalid, func(t *testing.T) {
			account := &Account{
				Platform: PlatformOpenAI,
				Type:     AccountTypeOAuth,
				Extra:    map[string]any{CodexFingerprintSeedExtraKey: invalid},
			}

			seed := EnsureCodexFingerprintSeed(account)

			require.NotEqual(t, invalid, seed)
			require.NoError(t, uuid.Validate(seed))
			require.NotEqual(t, uuid.Nil.String(), seed)
		})
	}
}

func TestShouldEnsureCodexFingerprintSeedForExtraUpdates(t *testing.T) {
	for _, mode := range []string{"device", "session", "cockpit", "full"} {
		require.True(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{
			codexFingerprintModeExtraKey: mode,
		}), mode)
	}
	require.False(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(nil))
	require.False(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{}))
	require.False(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{
		codexFingerprintModeExtraKey: "off",
	}))
}

func TestPrepareCodexFingerprintSeedForCreate_OnlyExplicitModesGenerate(t *testing.T) {
	for _, extra := range []map[string]any{
		nil,
		{},
		{codexFingerprintModeExtraKey: ""},
		{codexFingerprintModeExtraKey: "off"},
		{codexFingerprintModeExtraKey: "invalid"},
		{CodexFingerprintSeedExtraKey: uuid.NewString()},
	} {
		account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}

		require.Empty(t, PrepareCodexFingerprintSeedForCreate(account))
		require.NotContains(t, account.Extra, CodexFingerprintSeedExtraKey)
	}

	for _, mode := range []string{"device", "session", "cockpit", "full"} {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra:    map[string]any{codexFingerprintModeExtraKey: mode},
		}

		seed := PrepareCodexFingerprintSeedForCreate(account)
		require.NoError(t, uuid.Validate(seed), mode)
		require.Equal(t, seed, account.Extra[CodexFingerprintSeedExtraKey], mode)
	}
}

func TestPrepareCodexFingerprintSeedForCreate_RootRotatesIncomingShadowPreservesParent(t *testing.T) {
	incoming := uuid.NewString()
	root := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "session",
			CodexFingerprintSeedExtraKey: incoming,
		},
	}
	parentID := int64(7)
	shadow := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		Extra:           map[string]any{CodexFingerprintSeedExtraKey: incoming},
	}

	rootSeed := PrepareCodexFingerprintSeedForCreate(root)
	shadowSeed := PrepareCodexFingerprintSeedForCreate(shadow)

	require.NoError(t, uuid.Validate(rootSeed))
	assert.NotEqual(t, incoming, rootSeed, "新建根账号不得接受外部复用的种子")
	assert.Equal(t, incoming, shadowSeed, "影子账号必须继承父账号种子")
}

func TestPrepareCodexFingerprintSeedForCreate_ShadowWithoutParentSeedStaysEmptyWhenOff(t *testing.T) {
	parentID := int64(7)
	shadow := &Account{
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		ParentAccountID: &parentID,
		Extra:           map[string]any{CodexFingerprintSeedExtraKey: ""},
	}

	require.Empty(t, PrepareCodexFingerprintSeedForCreate(shadow))
	require.NotContains(t, shadow.Extra, CodexFingerprintSeedExtraKey)
}

// --- GetCodexFingerprintMode ---

func TestGetCodexFingerprintMode(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected codexFingerprintMode
	}{
		{"nil 账号", nil, codexFingerprintOff},
		{"非 OAuth 账号", &Account{Platform: PlatformOpenAI, Type: "api_key"}, codexFingerprintOff},
		{"无 extra 默认 off", newTestOAuthAccount(1, nil), codexFingerprintOff},
		{"空值默认 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: ""}), codexFingerprintOff},
		{"非法值默认 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "invalid"}), codexFingerprintOff},
		{"显式 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"}), codexFingerprintOff},
		{"device", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "device"}), codexFingerprintDevice},
		{"session", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"}), codexFingerprintSession},
		{"cockpit", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "cockpit"}), codexFingerprintCockpit},
		{"full", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "full"}), codexFingerprintFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.account.GetCodexFingerprintMode())
		})
	}
}

func TestExtractCockpitFingerprintSource_XClientRequestIDIsLastFallback(t *testing.T) {
	headers := make(http.Header)
	headers.Set("x-client-request-id", "per-request-uuid")
	headers.Set("session-id", "stable-session")
	body := map[string]any{"prompt_cache_key": "stable-cache"}
	source := extractCockpitFingerprintSource(headers, body)

	require.Empty(t, source.threadID)
	require.Equal(t, "stable-cache", source.promptCacheKey)
	require.Equal(t, "stable-session", source.originalSessionID)

	bareHeaders := make(http.Header)
	bareHeaders.Set("x-client-request-id", "fallback-request-id")
	bare := extractCockpitFingerprintSource(bareHeaders, map[string]any{})
	require.Equal(t, "fallback-request-id", bare.threadID)
}

func TestShouldUseOpenAICockpitAffinityRequiresCodexSignal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func(ua, originator string) *gin.Context {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("User-Agent", ua)
		c.Request.Header.Set("originator", originator)
		return c
	}

	require.False(t, ShouldUseOpenAICockpitAffinity(newContext("curl/8.5.0", ""), []byte(`{"model":"gpt-5.4"}`)))
	require.True(t, ShouldUseOpenAICockpitAffinity(newContext("Codex Desktop/0.145.0 (Windows)", ""), []byte(`{"model":"gpt-5.4"}`)))
	require.True(t, ShouldUseOpenAICockpitAffinity(newContext("curl/8.5.0", ""), []byte(`{"client_metadata":{"x-codex-installation-id":"install","thread_id":"thread"}}`)))
}

// --- resolveConvergedInstallationID ---

func TestResolveConvergedInstallationID_UsesDeviceID(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{"openai_device_id": "real-device-id"})
	assert.Equal(t, "real-device-id", resolveConvergedInstallationID(account))
}

func TestResolveConvergedInstallationID_DerivesFromPersistedSeed(t *testing.T) {
	account := newTestOAuthAccount(42, map[string]any{CodexFingerprintSeedExtraKey: uuid.NewString()})
	result := resolveConvergedInstallationID(account)
	_, err := uuid.Parse(result)
	require.NoError(t, err, "派生值应为合法 UUID")
	assert.Equal(t, result, resolveConvergedInstallationID(account), "确定性")
}

func TestResolveConvergedInstallationID_SameLocalIDDifferentSeeds(t *testing.T) {
	a := resolveConvergedInstallationID(newTestOAuthAccount(1, map[string]any{
		CodexFingerprintSeedExtraKey: uuid.NewString(),
	}))
	b := resolveConvergedInstallationID(newTestOAuthAccount(1, map[string]any{
		CodexFingerprintSeedExtraKey: uuid.NewString(),
	}))
	assert.NotEqual(t, a, b)
}

func TestResolveConvergedIDs_MissingSeedDoesNotFallbackToLocalID(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	assert.Empty(t, resolveConvergedInstallationID(account))
	assert.Empty(t, resolveConvergedSessionID(account))
	assert.Empty(t, resolveConvergedThreadID(account, "client-session"))
	assert.Empty(t, resolveConvergedPromptCacheKey(account, "cache-key"))
	assert.Nil(t, resolveCodexFingerprintIDsFromRequest(account, http.Header{"session-id": []string{"client-session"}}))

	account.Extra = map[string]any{"openai_device_id": "explicit-device"}
	assert.Nil(t, resolveCodexFingerprintIDsFromRequest(account, http.Header{"session-id": []string{"client-session"}}),
		"session 模式即使有显式设备 ID，也必须取得持久化种子后才能生成完整会话身份")
}

// --- resolveConvergedThreadID ---

func TestResolveConvergedThreadID_PerClientSession(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	a := resolveConvergedThreadID(account, "session-aaa")
	b := resolveConvergedThreadID(account, "session-bbb")
	assert.NotEqual(t, a, b, "不同客户端 session 应得到不同 thread_id")
}

func TestResolveConvergedThreadID_Deterministic(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	a := resolveConvergedThreadID(account, "session-aaa")
	b := resolveConvergedThreadID(account, "session-aaa")
	assert.Equal(t, a, b, "同一客户端 session 应得到相同 thread_id")
}

func TestResolveConvergedThreadID_EmptySession(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	assert.Equal(t, "", resolveConvergedThreadID(account, ""))
}

// --- resolveConvergedPromptCacheKey ---

func TestResolveConvergedPromptCacheKey_StableAndIsolated(t *testing.T) {
	accountA := newTestOAuthAccount(1, nil)
	accountB := newTestOAuthAccount(2, nil)

	a1 := resolveConvergedPromptCacheKey(accountA, "cache-A")
	a2 := resolveConvergedPromptCacheKey(accountA, "cache-A")
	assert.Equal(t, a1, a2, "同账号同缓存键应稳定")
	assert.NotEqual(t, a1, resolveConvergedPromptCacheKey(accountA, "cache-B"), "不同对话应隔离")
	assert.NotEqual(t, a1, resolveConvergedPromptCacheKey(accountB, "cache-A"), "不同账号应隔离")
	_, err := uuid.Parse(a1)
	require.NoError(t, err)
}

// --- off 模式：resolveCodexFingerprintIDsFromRequest 返回 nil ---

func TestResolveCodexFingerprintIDsFromRequest_ExplicitOff(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	assert.Nil(t, ids, "显式 off 模式应返回 nil")
}

func TestResolveCodexFingerprintIDsFromRequest_DefaultIsOff(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	assert.Nil(t, ids, "无 extra 时不应改写客户端身份")
}

// --- applyCodexFingerprintHeaders: off 模式 ---

func TestApplyCodexFingerprintHeaders_OffMode(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-installation-id", "original-install-id")
	h.Set("x-codex-window-id", "original-window-id")

	applyCodexFingerprintHeaders(h, nil)

	assert.Equal(t, "original-install-id", h.Get("x-codex-installation-id"), "nil ids 不改写")
	assert.Equal(t, "original-window-id", h.Get("x-codex-window-id"), "nil ids 不改写")
}

// --- applyCodexFingerprintHeaders: device 模式 ---

func TestApplyCodexFingerprintHeaders_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","sandbox":"seccomp"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-window:0")
	h.Set("x-codex-turn-metadata", turnMetadata)

	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, "converged-device", h.Get("x-codex-installation-id"), "installation_id 应收敛")
	assert.Equal(t, "user-window:0", h.Get("x-codex-window-id"), "device 模式不改写 window_id")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "user-session", meta["session_id"], "device 模式不改写 session_id")
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

// --- applyCodexFingerprintHeaders: session 模式 ---

func TestApplyCodexFingerprintHeaders_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","thread_id":"user-thread","turn_id":"user-turn","window_id":"user-thread:0","sandbox":"seccomp","thread_source":"user"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-thread:0")
	h.Set("x-codex-turn-metadata", turnMetadata)
	h.Set("x-client-request-id", "user-thread")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	applyCodexFingerprintHeaders(h, ids)

	convergedInstall := resolveConvergedInstallationID(account)
	convergedSession := resolveConvergedSessionID(account)
	convergedThread := resolveConvergedThreadID(account, "client-session-aaa")

	assert.Equal(t, convergedInstall, h.Get("x-codex-installation-id"))
	assert.Equal(t, convergedSession, h.Get("session-id"))
	assert.Equal(t, convergedSession, h.Get("session_id"), "下划线形式也应被改写")
	assert.Equal(t, convergedThread, h.Get("thread-id"))
	assert.Equal(t, convergedThread, h.Get("x-client-request-id"))
	assert.Equal(t, convergedThread+":0", h.Get("x-codex-window-id"))

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, convergedThread, meta["thread_id"])
	assert.NotEqual(t, "user-turn", meta["turn_id"], "turn_id 应被新生成的值替换")
	assert.Equal(t, "seccomp", meta["sandbox"], "sandbox 保留原样")
	assert.Equal(t, "user", meta["thread_source"], "thread_source 保留原样")
}

// --- session 模式：不同客户端得到不同 thread ---

func TestApplyCodexFingerprintHeaders_SessionMode_DifferentClients(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})

	makeTurnMeta := func() string {
		return `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`
	}

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("session-id"), hB.Get("session-id"), "session_id 应相同")
	assert.NotEqual(t, hA.Get("thread-id"), hB.Get("thread-id"), "不同客户端 thread_id 应不同")
	assert.NotEqual(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "不同客户端 window_id 应不同")
	assert.Equal(t, hA.Get("x-codex-installation-id"), hB.Get("x-codex-installation-id"))
}

// --- cockpit 模式 ---

func TestCockpitMode_UsesBodyFallbackAndRewritesPromptCacheKey(t *testing.T) {
	account := newTestOAuthAccount(7, map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
	})
	body := map[string]any{
		"prompt_cache_key": "client-cache-A",
		"client_metadata": map[string]any{
			"session_id":            "body-session-A",
			"thread_id":             "body-thread-A",
			"x-codex-window-id":     "body-thread-A:0",
			"x-codex-turn-metadata": `{"prompt_cache_key":"client-cache-A","turn_id":"client-turn","window_id":"body-thread-A:0"}`,
		},
	}

	ids := resolveCodexFingerprintIDsFromRequest(account, nil, body)
	require.NotNil(t, ids)
	assert.Equal(t, codexFingerprintCockpit, ids.mode)
	assert.Equal(t, resolveConvergedCockpitSessionID(account, "body-thread-A"), ids.sessionID)
	assert.Equal(t, resolveConvergedThreadID(account, "body-thread-A"), ids.threadID)
	expectedCacheKey := resolveConvergedPromptCacheKey(account, "client-cache-A")
	assert.Equal(t, expectedCacheKey, ids.promptCacheKey)

	require.True(t, applyCodexFingerprintClientMetadata(body, ids))
	assert.Equal(t, expectedCacheKey, body["prompt_cache_key"])
	clientMetadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ids.sessionID, clientMetadata["session_id"])
	assert.Equal(t, ids.threadID, clientMetadata["thread_id"])
	assert.Equal(t, ids.windowID, clientMetadata["x-codex-window-id"])

	turnMetadata, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var embedded map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetadata), &embedded))
	assert.Equal(t, expectedCacheKey, embedded["prompt_cache_key"])
	assert.Equal(t, ids.turnID, embedded["turn_id"])

	headers := http.Header{}
	headers.Set("conversation_id", "isolated-client-cache")
	headers.Set("x-codex-turn-metadata", `{"prompt_cache_key":"client-cache-A","turn_id":"client-turn"}`)
	applyCodexFingerprintHeaders(headers, ids)
	assert.Equal(t, expectedCacheKey, headers.Get("conversation_id"))
	assert.Equal(t, ids.sessionID, headers.Get("session-id"))
	assert.Equal(t, ids.threadID, headers.Get("thread-id"))

	var headerMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &headerMetadata))
	assert.Equal(t, expectedCacheKey, headerMetadata["prompt_cache_key"])
	assert.Equal(t, ids.turnID, headerMetadata["turn_id"])
}

func TestCockpitMode_PromptCacheFallbackKeepsConversationStable(t *testing.T) {
	account := newTestOAuthAccount(7, map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
	})
	bodyA := map[string]any{"prompt_cache_key": "cache-only"}
	bodyB := map[string]any{"prompt_cache_key": "cache-only"}

	idsA := resolveCodexFingerprintIDsFromRequest(account, nil, bodyA)
	idsB := resolveCodexFingerprintIDsFromRequest(account, nil, bodyB)
	require.NotNil(t, idsA)
	require.NotNil(t, idsB)
	assert.Equal(t, idsA.sessionID, idsB.sessionID, "相同缓存键应派生同一 session")
	assert.Equal(t, idsA.threadID, idsB.threadID, "相同缓存键应派生同一 thread")
	assert.Equal(t, idsA.promptCacheKey, idsB.promptCacheKey, "相同缓存键应稳定")
	assert.NotEqual(t, idsA.turnID, idsB.turnID, "不同请求仍应生成独立 turn")
}

func TestCockpitMode_ThreadSeedPrefersBodyThreadThenPromptCache(t *testing.T) {
	account := newTestOAuthAccount(9, map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
	})

	withThread := codexFingerprintSource{
		clientSessionID:   "shared-session",
		originalSessionID: "shared-session",
		threadID:          "body-thread",
		promptCacheKey:    "cache-a",
	}
	withoutThreadA := codexFingerprintSource{
		clientSessionID:   "shared-session",
		originalSessionID: "shared-session",
		promptCacheKey:    "cache-a",
	}
	withoutThreadB := codexFingerprintSource{
		clientSessionID:   "shared-session",
		originalSessionID: "shared-session",
		promptCacheKey:    "cache-b",
	}

	idsThread := resolveCodexFingerprintIDsWithSource(account, withThread, codexFingerprintCockpit)
	idsCacheA := resolveCodexFingerprintIDsWithSource(account, withoutThreadA, codexFingerprintCockpit)
	idsCacheB := resolveCodexFingerprintIDsWithSource(account, withoutThreadB, codexFingerprintCockpit)
	require.NotNil(t, idsThread)
	require.NotNil(t, idsCacheA)
	require.NotNil(t, idsCacheB)
	assert.Equal(t, resolveConvergedThreadID(account, "body-thread"), idsThread.threadID)
	assert.Equal(t, resolveConvergedThreadID(account, "cache-a"), idsCacheA.threadID)
	assert.Equal(t, resolveConvergedThreadID(account, "cache-b"), idsCacheB.threadID)
	assert.Equal(t, resolveConvergedCockpitSessionID(account, "body-thread"), idsThread.sessionID)
	assert.Equal(t, resolveConvergedCockpitSessionID(account, "cache-a"), idsCacheA.sessionID)
	assert.Equal(t, resolveConvergedCockpitSessionID(account, "cache-b"), idsCacheB.sessionID)
	assert.NotEqual(t, idsCacheA.sessionID, idsCacheB.sessionID)
	assert.NotEqual(t, idsCacheA.threadID, idsCacheB.threadID)
}

func TestCockpitMode_HeaderOnlyPromptCacheKeyDoesNotReinsertBodyField(t *testing.T) {
	account := newTestOAuthAccount(7, map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
	})
	ids := resolveCodexFingerprintIDsWithSource(account, codexFingerprintSource{
		clientSessionID:      "session-A",
		promptCacheKey:       "header-only-cache",
		promptCacheKeyInBody: false,
	}, codexFingerprintCockpit)
	require.NotNil(t, ids)

	body := map[string]any{}
	require.True(t, applyCodexFingerprintClientMetadata(body, ids))
	assert.NotContains(t, body, "prompt_cache_key", "Header-only 兼容键不应重新写回 Body")
}

// --- full 模式 ---

func TestApplyCodexFingerprintHeaders_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	convergedSession := resolveConvergedSessionID(account)

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("thread-id"), hB.Get("thread-id"), "full 模式 thread_id 应相同")
	assert.Equal(t, convergedSession, hA.Get("thread-id"), "full 模式 thread_id 应等于 session_id")
	assert.Equal(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "full 模式 window_id 应相同")
}

// --- H1 修复验证：头和体的 turn_id 一致性 ---

func TestFingerprintIDs_HeaderAndBody_TurnID_Consistent(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-xyz")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	// 头改写
	h := http.Header{}
	h.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(h, ids)

	// 体改写（使用同一份 ids）
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "x",
			"session_id":              "x",
			"turn_id":                 "x",
			"x-codex-turn-metadata":   `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}
	applyCodexFingerprintClientMetadata(reqBody, ids)

	// 从头 turn-metadata JSON 提取 turn_id
	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	headerTurnID, ok := headerMeta["turn_id"].(string)
	require.True(t, ok, "头 turn-metadata 应包含 string 类型的 turn_id")

	// 从体 client_metadata 提取 turn_id
	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok, "请求体应包含 client_metadata")
	bodyTurnID, ok := cm["turn_id"].(string)
	require.True(t, ok, "体 client_metadata 应包含 string 类型的 turn_id")

	// 从体内嵌 turn-metadata JSON 提取 turn_id
	embeddedRaw, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok, "体 client_metadata 应包含 x-codex-turn-metadata 字符串")
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(embeddedRaw), &bodyMeta))
	bodyEmbeddedTurnID, ok := bodyMeta["turn_id"].(string)
	require.True(t, ok, "体内嵌 turn-metadata 应包含 string 类型的 turn_id")

	assert.Equal(t, headerTurnID, bodyTurnID, "头和体的 turn_id 必须一致")
	assert.Equal(t, headerTurnID, bodyEmbeddedTurnID, "头和体内嵌 turn-metadata 的 turn_id 必须一致")
	assert.Equal(t, ids.turnID, headerTurnID, "所有 turn_id 都应来自同一份 ids")
}

// --- applyCodexFingerprintClientMetadata ---

func TestApplyCodexFingerprintClientMetadata_OffMode(t *testing.T) {
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original",
		},
	}
	modified := applyCodexFingerprintClientMetadata(reqBody, nil)
	assert.False(t, modified, "nil ids 不改写")
}

func TestApplyCodexFingerprintClientMetadata_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"user-session","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "user-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "converged-device", cm["x-codex-installation-id"])
	assert.Equal(t, "user-session", cm["session_id"], "device 模式不改 session_id")

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "original-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account)
	convergedSession := resolveConvergedSessionID(account)
	convergedThread := resolveConvergedThreadID(account, "client-session-aaa")

	assert.Equal(t, convergedInstall, cm["x-codex-installation-id"])
	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedThread, cm["thread_id"])
	assert.Equal(t, convergedThread+":0", cm["x-codex-window-id"])

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "any-client")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "x",
			"thread_id":             "x",
			"x-codex-turn-metadata": `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(account)

	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedSession, cm["thread_id"], "full 模式 thread_id 应等于 session_id")
}

// --- extractClientSessionID ---

func TestExtractClientSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		expected string
	}{
		{"连字符形式优先", func() http.Header {
			h := http.Header{}
			h.Set("session-id", "hyphen-form")
			h.Set("session_id", "underscore-form")
			return h
		}(), "hyphen-form"},
		{"回退到下划线形式", func() http.Header {
			h := http.Header{}
			h.Set("session_id", "underscore-form")
			return h
		}(), "underscore-form"},
		{"都没有", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractClientSessionID(tt.headers))
		})
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_MatchesDecodedPath(t *testing.T) {
	account := newTestOAuthAccount(7, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-session-raw", codexFingerprintSession)
	require.NotNil(t, ids)
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message"}],"client_metadata":{"session_id":"old","x-codex-turn-metadata":"{\"installation_id\":\"old\",\"sandbox\":\"seccomp\"}"}}`)

	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.True(t, applyCodexFingerprintClientMetadata(decoded, ids))

	rawBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	rawDecoded := map[string]any{}
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))

	assert.Equal(t, decoded["client_metadata"], rawDecoded["client_metadata"])
	assert.Equal(t, "gpt-5.6-sol", rawDecoded["model"])
}

func TestCockpitMode_RawBodyFallbackAndPromptCacheRewrite(t *testing.T) {
	account := newTestOAuthAccount(7, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"raw-cache","client_metadata":{"session_id":"raw-session","thread_id":"raw-thread","x-codex-turn-metadata":"{\"prompt_cache_key\":\"raw-cache\",\"turn_id\":\"raw-turn\"}"}}`)

	ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
	require.NotNil(t, ids)
	assert.Equal(t, resolveConvergedThreadID(account, "raw-thread"), ids.threadID)
	expectedCacheKey := resolveConvergedPromptCacheKey(account, "raw-cache")
	assert.Equal(t, expectedCacheKey, ids.promptCacheKey)

	updated, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)

	decoded := map[string]any{}
	require.NoError(t, json.Unmarshal(updated, &decoded))
	assert.Equal(t, expectedCacheKey, decoded["prompt_cache_key"])
	metadata, ok := decoded["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ids.sessionID, metadata["session_id"])
	assert.Equal(t, ids.threadID, metadata["thread_id"])
	assert.Equal(t, ids.windowID, metadata["x-codex-window-id"])
}

func TestCockpitMode_ExtractsOriginalIdentityFromEmbeddedTurnMetadata(t *testing.T) {
	account := newTestOAuthAccount(8, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":"{\"installation_id\":\"embedded-install\",\"session_id\":\"embedded-session\",\"thread_id\":\"embedded-thread\",\"turn_id\":\"embedded-turn\",\"window_id\":\"embedded-window\",\"prompt_cache_key\":\"embedded-cache\"}"}}`)

	ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
	require.NotNil(t, ids)
	assert.Equal(t, "embedded-install", ids.originalInstallationID)
	assert.Equal(t, "embedded-session", ids.originalSessionID)
	assert.Equal(t, "embedded-thread", ids.originalThreadID)
	assert.Equal(t, "embedded-turn", ids.originalTurnID)
	assert.Equal(t, "embedded-window", ids.originalWindowID)
	assert.Equal(t, "embedded-cache", ids.originalPromptCacheKey)
	assert.Equal(t, resolveConvergedThreadID(account, "embedded-thread"), ids.threadID)
	assert.Equal(t, resolveConvergedPromptCacheKey(account, "embedded-cache"), ids.promptCacheKey)
}

func TestStageCodexFingerprintIDs_NilClearsPriorAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	accountA := newTestOAuthAccount(11, map[string]any{codexFingerprintModeExtraKey: "session"})
	stageCodexFingerprintIDs(c, resolveCodexFingerprintIDs(accountA, "client-a", codexFingerprintSession))
	stageCodexFingerprintIDs(c, nil)

	headers := http.Header{}
	headers.Set("x-codex-installation-id", "client-installation")
	applyStagedCodexFingerprintHeaders(c, newTestOAuthAccount(12, nil), headers)
	assert.Equal(t, "client-installation", headers.Get("x-codex-installation-id"))
}

func TestApplyStagedCodexFingerprintHeaders_RequiresScheduledAccountOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	accountA := newTestOAuthAccount(31, map[string]any{codexFingerprintModeExtraKey: "session"})
	accountB := newTestOAuthAccount(32, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := bindCodexFingerprintIDsToAccount(
		resolveCodexFingerprintIDs(accountA, "client-a", codexFingerprintSession),
		accountA,
	)
	stageCodexFingerprintIDs(c, ids)

	foreignHeaders := http.Header{}
	foreignHeaders.Set("x-codex-installation-id", "foreign-original")
	applyStagedCodexFingerprintHeaders(c, accountB, foreignHeaders)
	assert.Equal(t, "foreign-original", foreignHeaders.Get("x-codex-installation-id"))

	ownedHeaders := http.Header{}
	applyStagedCodexFingerprintHeaders(c, accountA, ownedHeaders)
	assert.Equal(t, ids.installationID, ownedHeaders.Get("x-codex-installation-id"))

	zeroIDAccount := newTestOAuthAccount(0, map[string]any{codexFingerprintModeExtraKey: "session"})
	stageCodexFingerprintIDs(c, resolveCodexFingerprintIDs(zeroIDAccount, "client-zero", codexFingerprintSession))
	unboundHeaders := http.Header{}
	unboundHeaders.Set("x-codex-installation-id", "unbound-original")
	applyStagedCodexFingerprintHeaders(c, zeroIDAccount, unboundHeaders)
	assert.Equal(t, "unbound-original", unboundHeaders.Get("x-codex-installation-id"))
}

func TestRestoreCodexFingerprintResponsePayload_ExposesOriginalCockpitIdentity(t *testing.T) {
	account := newTestOAuthAccount(21, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	headers := http.Header{}
	headers.Set("session-id", "client-session")
	headers.Set("x-codex-installation-id", "client-installation")
	headers.Set("thread-id", "client-thread")
	headers.Set("x-codex-window-id", "client-window")
	headers.Set("x-codex-turn-metadata", `{"turn_id":"client-turn","window_id":"client-window"}`)
	body := map[string]any{
		"prompt_cache_key": "client-cache",
		"client_metadata": map[string]any{
			"thread_id":               "client-thread",
			"turn_id":                 "client-turn",
			"x-codex-window-id":       "client-window",
			"x-codex-installation-id": "client-installation",
		},
	}

	ids := resolveCodexFingerprintIDsFromRequest(account, headers, body)
	require.NotNil(t, ids)
	require.Equal(t, "client-installation", ids.originalInstallationID)
	require.Equal(t, "client-session", ids.originalSessionID)
	require.Equal(t, "client-thread", ids.originalThreadID)
	require.Equal(t, "client-turn", ids.originalTurnID)
	require.Equal(t, "client-window", ids.originalWindowID)
	require.Equal(t, "client-cache", ids.originalPromptCacheKey)

	upstreamPayload, err := json.Marshal(map[string]any{
		"installation_id":  ids.installationID,
		"session_id":       ids.sessionID,
		"thread_id":        ids.threadID,
		"turn_id":          ids.turnID,
		"window_id":        ids.windowID,
		"prompt_cache_key": ids.promptCacheKey,
	})
	require.NoError(t, err)
	restored := restoreCodexFingerprintResponsePayload(upstreamPayload, ids)

	assert.JSONEq(t, `{
		"installation_id":"client-installation",
		"session_id":"client-session",
		"thread_id":"client-thread",
		"turn_id":"client-turn",
		"window_id":"client-window",
		"prompt_cache_key":"client-cache"
	}`, string(restored))
}

func TestRestoreCodexFingerprintResponsePayload_WorksForSSEAndOffMode(t *testing.T) {
	ids := &codexFingerprintIDs{
		mode:              codexFingerprintCockpit,
		originalSessionID: "client-session",
		sessionID:         "upstream-session",
	}
	payload := []byte("data: {\"session_id\":\"upstream-session\"}\n\n")
	restored := restoreCodexFingerprintResponsePayload(payload, ids)
	assert.Equal(t, "data: {\"session_id\":\"client-session\"}\n\n", string(restored))
	assert.Equal(t, "plain client-session error", string(restoreCodexFingerprintResponsePayload([]byte("plain upstream-session error"), ids)))
	assert.Equal(t, payload, restoreCodexFingerprintResponsePayload(payload, nil))
}

func TestRestoreCodexFingerprintResponsePayload_FullModeSeparatesSessionAndThread(t *testing.T) {
	ids := &codexFingerprintIDs{
		mode:              codexFingerprintFull,
		originalSessionID: "client-session",
		sessionID:         "shared-upstream-id",
		originalThreadID:  "client-thread",
		threadID:          "shared-upstream-id",
	}
	payload := []byte(`{"session_id":"shared-upstream-id","thread_id":"shared-upstream-id","other":"shared-upstream-id","large_number":9007199254740993,"x-codex-turn-metadata":"{\"session_id\":\"shared-upstream-id\",\"thread_id\":\"shared-upstream-id\"}"}`)

	restored := restoreCodexFingerprintResponsePayload(payload, ids)

	assert.JSONEq(t, `{"session_id":"client-session","thread_id":"client-thread","other":"shared-upstream-id","large_number":9007199254740993,"x-codex-turn-metadata":"{\"session_id\":\"client-session\",\"thread_id\":\"client-thread\"}"}`, string(restored))
	assert.Contains(t, string(restored), `"large_number":9007199254740993`, "恢复身份时不得损失大整数精度")
}

func TestRestoreStagedCodexFingerprintResponsePayload_UsesCurrentFailoverAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	first := &codexFingerprintIDs{mode: codexFingerprintSession, originalSessionID: "client-first", sessionID: "upstream-first"}
	second := &codexFingerprintIDs{mode: codexFingerprintSession, originalSessionID: "client-second", sessionID: "upstream-second"}

	stageCodexFingerprintIDs(c, first)
	stageCodexFingerprintIDs(c, second)
	restored := restoreStagedCodexFingerprintResponsePayload(c, []byte(`{"session_id":"upstream-second","other":"upstream-first"}`))

	assert.JSONEq(t, `{"session_id":"client-second","other":"upstream-first"}`, string(restored))
}
