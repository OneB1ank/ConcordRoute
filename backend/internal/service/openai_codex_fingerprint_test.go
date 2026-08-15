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
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
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

func TestCodexFingerprintStableAccountID_ShadowUsesParent(t *testing.T) {
	parentID := int64(42)
	shadow := newTestOAuthAccount(99, nil)
	shadow.ParentAccountID = &parentID
	parent := newTestOAuthAccount(parentID, nil)

	assert.Equal(t, parentID, codexFingerprintStableAccountID(shadow))
	assert.Equal(t, resolveConvergedInstallationID(parent), resolveConvergedInstallationID(shadow))
	assert.Equal(t, resolveConvergedSessionID(parent), resolveConvergedSessionID(shadow))
	assert.Equal(t, resolveConvergedThreadID(parent, "client-session"), resolveConvergedThreadID(shadow, "client-session"))
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
		{"无 extra 默认 session", newTestOAuthAccount(1, nil), codexFingerprintSession},
		{"空值默认 session", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: ""}), codexFingerprintSession},
		{"非法值默认 session", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "invalid"}), codexFingerprintSession},
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

// --- resolveConvergedInstallationID ---

func TestResolveConvergedInstallationID_UsesDeviceID(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{"openai_device_id": "real-device-id"})
	assert.Equal(t, "real-device-id", resolveConvergedInstallationID(account))
}

func TestResolveConvergedInstallationID_DerivesFromAccountID(t *testing.T) {
	account := newTestOAuthAccount(42, nil)
	result := resolveConvergedInstallationID(account)
	_, err := uuid.Parse(result)
	require.NoError(t, err, "派生值应为合法 UUID")
	assert.Equal(t, result, resolveConvergedInstallationID(account), "确定性")
}

func TestResolveConvergedInstallationID_DifferentAccounts(t *testing.T) {
	a := resolveConvergedInstallationID(newTestOAuthAccount(1, nil))
	b := resolveConvergedInstallationID(newTestOAuthAccount(2, nil))
	assert.NotEqual(t, a, b)
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

func TestResolveCodexFingerprintIDsFromRequest_DefaultIsSession(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids, "无 extra 默认 session 模式，应返回非 nil")
	assert.Equal(t, codexFingerprintSession, ids.mode)
	assert.NotEmpty(t, ids.sessionID)
	assert.NotEmpty(t, ids.turnID)
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
	assert.Equal(t, idsA.threadID, idsB.threadID, "相同缓存键应派生同一 thread")
	assert.Equal(t, idsA.promptCacheKey, idsB.promptCacheKey, "相同缓存键应稳定")
	assert.NotEqual(t, idsA.turnID, idsB.turnID, "不同请求仍应生成独立 turn")
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
