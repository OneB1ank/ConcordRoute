package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Cockpit 只收敛会话、线程和窗口；客户端回合图及其开始时间保持原值。
func TestCockpitTurnPassthroughDecodedRequest(t *testing.T) {
	account := newTestOAuthAccount(194001, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	const (
		turn    = "01995000-0000-7000-8000-000000000101"
		parent  = "01995000-0000-7000-8000-000000000100"
		started = int64(1790000000123)
	)
	body := map[string]any{
		"parent_turn_id": parent,
		"root_turn_id":   parent,
		"client_metadata": map[string]any{
			"session_id":              "client-session",
			"thread_id":               "client-thread",
			"turn_id":                 turn,
			"turn_started_at_unix_ms": started,
			"x-codex-turn-metadata":   `{"turn_id":"` + turn + `","parent_turn_id":"` + parent + `","root_turn_id":"` + parent + `","turn_started_at_unix_ms":1790000000123}`,
		},
	}

	ids := resolveCodexFingerprintIDsFromRequest(account, nil, body)
	require.NotNil(t, ids)
	require.NotEqual(t, "client-session", ids.sessionID)
	require.NotEqual(t, "client-thread", ids.threadID)
	require.Equal(t, turn, ids.turnID)
	require.Equal(t, parent, ids.parentTurnID)
	require.Equal(t, parent, ids.rootTurnID)
	require.Equal(t, started, ids.turnStartedAtUnixMS)

	require.True(t, applyCodexFingerprintClientMetadata(body, ids))
	metadata := body["client_metadata"].(map[string]any)
	require.Equal(t, turn, metadata["turn_id"])
	require.Equal(t, parent, metadata["parent_turn_id"])
	require.Equal(t, parent, metadata["root_turn_id"])
	require.EqualValues(t, started, metadata["turn_started_at_unix_ms"])
	nested := metadata["x-codex-turn-metadata"].(string)
	require.Equal(t, turn, gjson.Get(nested, "turn_id").String())
	require.Equal(t, parent, gjson.Get(nested, "parent_turn_id").String())
	require.Equal(t, parent, gjson.Get(nested, "root_turn_id").String())
	require.Equal(t, started, gjson.Get(nested, "turn_started_at_unix_ms").Int())
}

// OAuth 原始透传路径只局部改写身份对象，回合字段不得换值。
func TestCockpitTurnPassthroughRawRequest(t *testing.T) {
	account := newTestOAuthAccount(194002, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	raw := []byte(`{"model":"gpt-6-astra","client_metadata":{"session_id":"raw-session","thread_id":"raw-thread","turn_id":"raw-turn","parent_turn_id":"raw-parent","root_turn_id":"raw-root","turn_started_at_unix_ms":"1790000000456","x-codex-turn-metadata":"{\"turn_id\":\"raw-turn\",\"parent_turn_id\":\"raw-parent\",\"root_turn_id\":\"raw-root\",\"turn_started_at_unix_ms\":1790000000456}"},"input":[{"role":"user","content":"keep"}]}`)

	ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	require.NotNil(t, ids)
	require.Equal(t, "raw-turn", ids.turnID)
	require.Equal(t, "raw-parent", ids.parentTurnID)
	require.Equal(t, "raw-root", ids.rootTurnID)

	wire, changed, err := applyCodexFingerprintClientMetadataRaw(raw, ids)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "raw-turn", gjson.GetBytes(wire, "client_metadata.turn_id").String())
	require.Equal(t, "raw-parent", gjson.GetBytes(wire, "client_metadata.parent_turn_id").String())
	require.Equal(t, "raw-root", gjson.GetBytes(wire, "client_metadata.root_turn_id").String())
	require.Equal(t, "1790000000456", gjson.GetBytes(wire, "client_metadata.turn_started_at_unix_ms").String())
	nested := gjson.GetBytes(wire, "client_metadata.x-codex-turn-metadata").String()
	require.Equal(t, "raw-turn", gjson.Get(nested, "turn_id").String())
	require.Equal(t, int64(1790000000456), gjson.Get(nested, "turn_started_at_unix_ms").Int())
	require.Equal(t, "keep", gjson.GetBytes(wire, "input.0.content").String())
}

// WebSocket 每帧使用本帧的回合字段；当前帧缺失时不回灌上一帧。
func TestCockpitTurnPassthroughWebSocketFrames(t *testing.T) {
	account := newTestOAuthAccount(194003, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	firstBody := []byte(`{"client_metadata":{"session_id":"ws-session","thread_id":"ws-thread","turn_id":"ws-turn-1","turn_started_at_unix_ms":1000}}`)
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, firstBody)
	require.NotNil(t, first)

	secondBody := []byte(`{"client_metadata":{"session_id":"ws-session","thread_id":"ws-thread","turn_id":"ws-turn-2","parent_turn_id":"ws-turn-1","root_turn_id":"ws-turn-1","turn_started_at_unix_ms":2000}}`)
	second := advanceCodexWebSocketFingerprint(account, first, secondBody)
	require.Equal(t, "ws-turn-2", second.turnID)
	require.Equal(t, "ws-turn-1", second.parentTurnID)
	require.Equal(t, "ws-turn-1", second.rootTurnID)
	require.Equal(t, int64(2000), second.turnStartedAtUnixMS)

	wire, _, err := applyCodexFingerprintClientMetadataRaw(secondBody, second)
	require.NoError(t, err)
	require.Equal(t, "ws-turn-2", gjson.GetBytes(wire, "client_metadata.turn_id").String())
	require.Equal(t, int64(2000), gjson.GetBytes(wire, "client_metadata.turn_started_at_unix_ms").Int())

	missingBody := []byte(`{"client_metadata":{"session_id":"ws-session","thread_id":"ws-thread"}}`)
	missing := advanceCodexWebSocketFingerprint(account, second, missingBody)
	wire, _, err = applyCodexFingerprintClientMetadataRaw(missingBody, missing)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(wire, "client_metadata.turn_id").Exists())
	require.False(t, gjson.GetBytes(wire, "client_metadata.parent_turn_id").Exists())
	require.False(t, gjson.GetBytes(wire, "client_metadata.root_turn_id").Exists())
	require.False(t, gjson.GetBytes(wire, "client_metadata.turn_started_at_unix_ms").Exists())
}

// 旧 compact 的 Header-only 路径仍收敛会话，但回合元数据保持客户端值。
func TestCockpitTurnPassthroughHeaderOnlyCompact(t *testing.T) {
	account := newTestOAuthAccount(194004, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	headers := http.Header{}
	headers.Set("session-id", "compact-session")
	headers.Set("thread-id", "compact-thread")
	headers.Set("x-codex-turn-metadata", `{"turn_id":"compact-turn","parent_turn_id":"compact-parent","root_turn_id":"compact-root","turn_started_at_unix_ms":1790000000789}`)

	ids := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{}`), false)
	require.NotNil(t, ids)
	applyCodexFingerprintHeaders(headers, ids)
	metadata := headers.Get("x-codex-turn-metadata")
	require.Equal(t, "compact-turn", gjson.Get(metadata, "turn_id").String())
	require.Equal(t, "compact-parent", gjson.Get(metadata, "parent_turn_id").String())
	require.Equal(t, "compact-root", gjson.Get(metadata, "root_turn_id").String())
	require.Equal(t, int64(1790000000789), gjson.Get(metadata, "turn_started_at_unix_ms").Int())
	require.NotEqual(t, "compact-session", headers.Get("session-id"))
	require.NotEqual(t, "compact-thread", headers.Get("thread-id"))
}

// session 模式继续生成网关回合 ID，避免 Cockpit 调整改变其他模式。
func TestCockpitTurnPassthroughDoesNotChangeSessionMode(t *testing.T) {
	account := newTestOAuthAccount(194005, map[string]any{codexFingerprintModeExtraKey: "session"})
	body := map[string]any{"client_metadata": map[string]any{"session_id": "session-mode", "turn_id": "client-turn"}}
	ids := resolveCodexFingerprintIDsFromRequest(account, nil, body)
	require.NotNil(t, ids)
	require.NotEqual(t, "client-turn", ids.turnID)
}

// 升级前遗留的回合映射只作为旧数据保留，不得改变新请求的客户端 turn。
func TestCockpitTurnPassthroughIgnoresLegacyLineageBindings(t *testing.T) {
	account := newTestOAuthAccount(194006, map[string]any{
		codexFingerprintModeExtraKey: "cockpit",
		CodexTurnLineageBindingsExtraKey: map[string]any{
			"legacy-binding": codexIdentityBinding{UUID: "01995000-0000-7000-8000-000000000999"},
		},
	})
	raw := []byte(`{"client_metadata":{"session_id":"legacy-session","thread_id":"legacy-thread","turn_id":"legacy-turn"}}`)
	ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	require.NotNil(t, ids)
	require.Equal(t, "legacy-turn", ids.turnID)
	require.Len(t, readCodexTurnLineageBindings(account), 1)
}
