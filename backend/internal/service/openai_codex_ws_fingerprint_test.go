package service

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 验证 A → 缺省 → B，以及压缩后缺省键不跨窗口继承；输入帧不被原地修改。
func TestAdvanceCodexWebSocketFingerprintCacheLifecycle(t *testing.T) {
	account := newTestOAuthAccount(1210, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, []byte(`{"prompt_cache_key":"ws-A","client_metadata":{"session_id":"ws-session","thread_id":"ws-thread","turn_id":"ws-turn-1","x-codex-window-id":"ws-thread:0"}}`))
	require.NotNil(t, first)
	snapshot := *first
	missing := advanceCodexWebSocketFingerprint(account, first, []byte(`{"model":"gpt-6-astra"}`))
	require.Equal(t, first.promptCacheKey, missing.promptCacheKey)
	require.Equal(t, first.turnID, missing.turnID)
	second := advanceCodexWebSocketFingerprint(account, missing, []byte(`{"prompt_cache_key":"ws-B","client_metadata":{"turn_id":"ws-turn-2"}}`))
	assert.Equal(t, "ws-B", second.promptCacheKey)
	assert.Equal(t, first.threadID, second.threadID)
	assert.Equal(t, first.contextWindowID, second.contextWindowID)
	assert.NotEqual(t, first.turnID, second.turnID)
	assert.Equal(t, second.turnID, second.rootTurnID)
	assert.Equal(t, snapshot, *first, "帧身份更新不得污染握手快照")

	retry := advanceCodexWebSocketFingerprint(account, second, []byte(`{"prompt_cache_key":"ws-B","client_metadata":{"turn_id":"ws-turn-2"}}`))
	assert.Equal(t, second.turnID, retry.turnID)
	assert.Equal(t, second.turnStartedAtUnixMS, retry.turnStartedAtUnixMS)
	compacted := advanceCodexWebSocketFingerprint(account, retry, []byte(`{"client_metadata":{"window_number":"1","turn_id":"ws-turn-3"}}`))
	assert.Equal(t, first.firstWindowID, compacted.firstWindowID)
	assert.Equal(t, first.contextWindowID, compacted.previousWindowID)
	assert.NotEqual(t, "ws-B", compacted.promptCacheKey)
	assert.False(t, compacted.promptCacheKeyInBody)
	assert.Equal(t, first.sessionID, compacted.sessionID)
	assert.Equal(t, first.threadID, compacted.threadID)
	assert.Equal(t, snapshot, *first)
}

// 连续压缩与多个账号使用同一客户端标记，验证链关系与账号隔离保持成立。
func TestAdvanceCodexWebSocketFingerprintWindowChainAndAccountIsolation(t *testing.T) {
	var otherFirst string
	for _, accountID := range []int64{1211, 1212} {
		account := newTestOAuthAccount(accountID, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
		ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, []byte(`{"client_metadata":{"session_id":"shared-session","thread_id":"shared-thread"}}`))
		require.NotNil(t, ids)
		first := ids.contextWindowID
		assert.NotEqual(t, otherFirst, first)
		otherFirst = first
		for generation := 1; generation <= 8; generation++ {
			body := []byte(fmt.Sprintf(`{"prompt_cache_key":"cache-%d","client_metadata":{"window_number":"%d","turn_id":"turn-%d"}}`, generation, generation, generation))
			next := advanceCodexWebSocketFingerprint(account, ids, body)
			assert.Equal(t, first, next.firstWindowID)
			assert.Equal(t, ids.contextWindowID, next.previousWindowID)
			assert.NotEqual(t, ids.contextWindowID, next.contextWindowID)
			updated, _, err := applyCodexFingerprintClientMetadataRaw(body, next)
			require.NoError(t, err)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(updated, &decoded))
			for _, key := range codexMetadataOnlyBodyFields {
				assert.NotContains(t, decoded, key)
			}
			ids = next
		}
	}
}

// 会话切换不能只因窗口号相同就沿用旧键；失败帧也不应污染当前快照。
func TestCodexWebSocketFingerprintStateRejectsCrossConversation(t *testing.T) {
	account := newTestOAuthAccount(1213, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	firstBody := []byte(`{"prompt_cache_key":"isolated-cache","client_metadata":{"session_id":"source-session","thread_id":"source-thread"}}`)
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, firstBody)
	require.NotNil(t, first)
	state := newCodexWebSocketFingerprintState(account, first, firstBody)
	for _, raw := range []string{
		`{"client_metadata":{"session_id":"other-session"}}`,
		`{"client_metadata":{"thread_id":"other-thread"}}`,
	} {
		_, err := state.advance([]byte(raw))
		require.ErrorContains(t, err, "reconnect")
		assert.Same(t, first, state.current)
	}
	carried, err := state.advance([]byte(`{"model":"gpt-6-astra"}`))
	require.NoError(t, err)
	assert.Equal(t, first.promptCacheKey, carried.promptCacheKey)
}
