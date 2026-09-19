package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 缺失和异常配置保持透传，避免升级自动开启身份改写。
func TestCodexTurnModeDefaults(t *testing.T) {
	for _, value := range []any{nil, "", "passthrough", "unknown", "CONVERGE", true, 1} {
		account := newTestOAuthAccount(99001, map[string]any{codexTurnModeExtraKey: value})
		require.Equal(t, codexTurnPassthrough, account.GetCodexTurnMode())
	}
	require.Equal(t, codexTurnPassthrough, (*Account)(nil).GetCodexTurnMode())
	account := newTestOAuthAccount(99002, map[string]any{codexTurnModeExtraKey: "converge"})
	require.Equal(t, codexTurnConverge, account.GetCodexTurnMode())
	account.Type = AccountTypeAPIKey
	require.Equal(t, codexTurnPassthrough, account.GetCodexTurnMode())
}

// 两种模式共用头、解析后载荷、原始载荷和 WS 帧；只切换回合图策略。
func TestCodexTurnModeCarriers(t *testing.T) {
	for _, mode := range []string{"", "passthrough", "converge"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(9990000+codexSnapshotTestAccountID.Add(1), map[string]any{
				codexFingerprintModeExtraKey: "cockpit", codexTurnModeExtraKey: mode,
			})
			t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
			raw := []byte(`{"model":"test-model","prompt_cache_key":"keep-cache","client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","parent_turn_id":"parent","root_turn_id":"parent","turn_started_at_unix_ms":"1000","x-codex-turn-metadata":"{\"turn_id\":\"turn\",\"parent_turn_id\":\"parent\",\"root_turn_id\":\"parent\"}"},"input":"keep-input"}`)
			var body map[string]any
			require.NoError(t, json.Unmarshal(raw, &body))
			ids := resolveCodexFingerprintIDsFromRequest(account, nil, body)
			rawIDs := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
			require.Equal(t, ids.turnID, rawIDs.turnID)
			require.Equal(t, ids.parentTurnID, rawIDs.parentTurnID)
			require.Equal(t, ids.parentTurnID, ids.rootTurnID)
			if mode == "converge" {
				require.NotEqual(t, "turn", ids.turnID)
				require.NotEmpty(t, readCodexTurnLineageBindings(account))
			} else {
				require.Equal(t, "turn", ids.turnID)
				require.Equal(t, "parent", ids.parentTurnID)
				require.Empty(t, readCodexTurnLineageBindings(account))
			}
			require.True(t, applyCodexFingerprintClientMetadata(body, ids))
			wire, _, err := applyCodexFingerprintClientMetadataRaw(raw, rawIDs)
			require.NoError(t, err)
			headers := http.Header{"X-Codex-Turn-Metadata": []string{`{"turn_id":"turn","parent_turn_id":"parent","root_turn_id":"parent"}`}}
			applyCodexFingerprintHeaders(headers, ids)
			require.Equal(t, ids.turnID, gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "turn_id").String())
			require.Equal(t, ids.turnID, gjson.GetBytes(wire, "client_metadata.turn_id").String())
			require.Equal(t, ids.turnID, body["client_metadata"].(map[string]any)["turn_id"])
			require.Equal(t, "keep-cache", gjson.GetBytes(wire, "prompt_cache_key").String())
			require.Equal(t, "keep-input", gjson.GetBytes(wire, "input").String())
			require.EqualValues(t, 1000, ids.turnStartedAtUnixMS)

			state := newCodexWebSocketFingerprintState(account, ids, nil, raw)
			repeated, err := state.advance(raw)
			require.NoError(t, err)
			require.Equal(t, ids.turnID, repeated.turnID)
			// 热改设置不拆开现有 WS 连接内的回合映射图。
			account.Extra[codexTurnModeExtraKey] = "converge"
			repeated, err = state.advance(raw)
			require.NoError(t, err)
			require.Equal(t, ids.turnID, repeated.turnID)
			missing, err := state.advance([]byte(`{"client_metadata":{}}`))
			require.NoError(t, err)
			require.Empty(t, missing.turnID)
			require.Empty(t, missing.parentTurnID)
			require.Empty(t, missing.rootTurnID)
			require.False(t, missing.turnStartedAtPresent)
		})
	}
}

// 配置切换只改变新请求的回合策略，不旋转已有会话、窗口或缓存键。
func TestCodexTurnModeSwitchIsolation(t *testing.T) {
	account := newTestOAuthAccount(9990000+codexSnapshotTestAccountID.Add(1), map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
	})
	t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
	raw := []byte(`{"prompt_cache_key":"keep","client_metadata":{"session_id":"s","thread_id":"t","turn_id":"turn"}}`)
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	key := normalizeOpenAIWSHandshakeCompatibility(account, nil)
	account.Extra[codexTurnModeExtraKey] = "converge"
	mapped := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	require.NotEqual(t, first.turnID, mapped.turnID)
	require.NotEqual(t, key, normalizeOpenAIWSHandshakeCompatibility(account, nil))
	require.Equal(t, first.sessionID, mapped.sessionID)
	require.Equal(t, first.threadID, mapped.threadID)
	require.Equal(t, first.windowID, mapped.windowID)
	require.Equal(t, first.promptCacheKey, mapped.promptCacheKey)
	account.Extra[codexTurnModeExtraKey] = "passthrough"
	last := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	require.Equal(t, first.turnID, last.turnID)
	require.Equal(t, key, normalizeOpenAIWSHandshakeCompatibility(account, nil))
}
