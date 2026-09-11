package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 子回合标识属于元数据；不得提升为 Responses 不支持的顶层生成参数。
func TestCodexFingerprintParentTurnMetadataContract(t *testing.T) {
	const parent = "01993000-0000-7000-8000-000000000001"
	const root = "01993000-0000-7000-8000-000000000002"
	for _, mode := range []string{"session", "cockpit", "full"} {
		for _, carrier := range []string{"body", "client_metadata", "embedded", "header"} {
			t.Run(mode+"/"+carrier, func(t *testing.T) {
				account := newTestOAuthAccount(1201, map[string]any{codexFingerprintModeExtraKey: mode})
				headers := make(http.Header)
				headers.Set("User-Agent", "Codex Desktop/0.153.4 (Mac OS 26.5.2; arm64)")
				headers.Set("session-id", "parent-contract-session")
				body := map[string]any{
					"model": "gpt-6-astra", "input": "parent-contract-input", "prompt_cache_key": "parent-cache",
				}
				lineage := map[string]any{"parent_turn_id": parent, "root_turn_id": root, "unrelated": "preserved"}
				encoded, err := json.Marshal(lineage)
				require.NoError(t, err)
				switch carrier {
				case "body":
					body["parent_turn_id"], body["root_turn_id"] = parent, root
				case "client_metadata":
					body["client_metadata"] = lineage
				case "embedded":
					body["client_metadata"] = map[string]any{"x-codex-turn-metadata": string(encoded)}
				case "header":
					headers.Set("x-codex-turn-metadata", string(encoded))
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				ids := resolveCodexFingerprintIDsFromRequest(account, headers, body)
				require.NotNil(t, ids)
				require.True(t, applyCodexFingerprintClientMetadata(body, ids))
				expectedParent, expectedRoot := parent, root
				if mode == "cockpit" {
					expectedParent, expectedRoot = ids.parentTurnID, ids.rootTurnID
					requireCodexUUIDv7(t, expectedParent)
					requireCodexUUIDv7(t, expectedRoot)
					assert.NotEqual(t, parent, expectedParent)
					assert.NotEqual(t, root, expectedRoot)
				}
				assertParentTurnMetadataContract(t, body, expectedParent, expectedRoot)

				rawIDs := resolveCodexFingerprintIDsFromRawRequest(account, headers, raw)
				require.NotNil(t, rawIDs)
				updated, changed, err := applyCodexFingerprintClientMetadataRaw(raw, rawIDs)
				require.NoError(t, err)
				require.True(t, changed)
				var decoded map[string]any
				require.NoError(t, json.Unmarshal(updated, &decoded))
				expectedRawParent, expectedRawRoot := parent, root
				if mode == "cockpit" {
					expectedRawParent, expectedRawRoot = rawIDs.parentTurnID, rawIDs.rootTurnID
				}
				assertParentTurnMetadataContract(t, decoded, expectedRawParent, expectedRawRoot)
			})
		}
	}
}

// 官方 client_metadata 是字符串字典，只有内嵌 turn metadata 的 window_number 是数字。
func TestCodexFingerprintWindowWireContract(t *testing.T) {
	account := newTestOAuthAccount(1202, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	for _, generation := range []int{0, 2} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			embedded := fmt.Sprintf(`{"window_number":%d,"previous_window_id":"stale-previous","first_window_id":"original-first","extra":"keep"}`, generation)
			body := map[string]any{
				"context_window_id": "original-current", "window_number": float64(generation),
				"first_window_id": "original-first", "previous_window_id": "stale-previous",
				"client_metadata": map[string]any{
					"session_id": "wire-session", "thread_id": "wire-thread", "window_number": fmt.Sprint(generation),
					"previous_window_id": "stale-previous", "x-codex-turn-metadata": embedded,
				},
			}
			headers := make(http.Header)
			headers.Set("x-codex-turn-metadata", embedded)
			ids := resolveCodexFingerprintIDsFromRequest(account, headers, body)
			require.NotNil(t, ids)
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			require.True(t, applyCodexFingerprintClientMetadata(body, ids))
			updated, changed, err := applyCodexFingerprintClientMetadataRaw(raw, ids)
			require.NoError(t, err)
			require.True(t, changed)
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(updated, &decoded))
			assert.Equal(t, body, decoded, "普通与透传路径必须输出相同的元数据结构")
			for _, key := range []string{"root_turn_id", "parent_turn_id", "context_window_id", "window_number", "first_window_id", "previous_window_id"} {
				assert.NotContains(t, decoded, key, "扩展身份字段不得进入 Responses 顶层")
			}
			var wire struct {
				ClientMetadata map[string]string `json:"client_metadata"`
			}
			require.NoError(t, json.Unmarshal(updated, &wire), "须满足官方客户端字符串元数据契约")
			assert.Equal(t, fmt.Sprint(generation), wire.ClientMetadata["window_number"])
			assert.Equal(t, ids.firstWindowID, wire.ClientMetadata["first_window_id"])
			var nested map[string]any
			require.NoError(t, json.Unmarshal([]byte(wire.ClientMetadata["x-codex-turn-metadata"]), &nested))
			assert.Equal(t, float64(generation), nested["window_number"])
			assert.Equal(t, "keep", nested["extra"])
			applyCodexFingerprintHeaders(headers, ids)
			var headerMetadata map[string]any
			require.NoError(t, json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &headerMetadata))
			if generation == 0 {
				assert.NotContains(t, wire.ClientMetadata, "previous_window_id")
				assert.NotContains(t, nested, "previous_window_id")
				assert.NotContains(t, headerMetadata, "previous_window_id")
			} else {
				assert.Equal(t, ids.previousWindowID, wire.ClientMetadata["previous_window_id"])
				assert.Equal(t, ids.previousWindowID, nested["previous_window_id"])
				assert.Equal(t, ids.previousWindowID, headerMetadata["previous_window_id"])
			}
		})
	}
}

// JSON 的整数、小数及指数写法应在解码体和原始体路径中获得相同窗口代数。
func TestCodexFingerprintWindowNumberPathParity(t *testing.T) {
	account := newTestOAuthAccount(1203, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	for _, tc := range []struct {
		literal string
		want    uint64
		present bool
	}{
		{"2", 2, true}, {"2.0", 2, true}, {"2e0", 2, true}, {`"2"`, 2, true},
		{"0", 0, true}, {"-1", 0, false}, {"1.5", 0, false}, {"null", 0, false},
		{"9007199254740992", 0, false}, {`"18446744073709551615"`, 0, false},
	} {
		t.Run(tc.literal, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"client_metadata":{"session_id":"number-session","thread_id":"number-thread","x-codex-window-id":"number-thread:1","window_number":%s}}`, tc.literal))
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(raw, &decoded))
			fromMap := extractCockpitFingerprintSource(nil, decoded)
			fromRaw := extractCockpitFingerprintSourceRaw(nil, raw)
			assert.Equal(t, tc.present, fromMap.windowNumberPresent)
			assert.Equal(t, tc.present, fromRaw.windowNumberPresent)
			if tc.present {
				assert.Equal(t, tc.want, fromMap.windowNumber)
				assert.Equal(t, tc.want, fromRaw.windowNumber)
			}
			mapIDs := resolveCodexFingerprintIDsFromRequest(account, nil, decoded)
			rawIDs := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
			require.NotNil(t, mapIDs)
			require.NotNil(t, rawIDs)
			assert.Equal(t, mapIDs.windowID, rawIDs.windowID)
			assert.Equal(t, mapIDs.contextWindowID, rawIDs.contextWindowID)
			assert.Equal(t, mapIDs.firstWindowID, rawIDs.firstWindowID)
			assert.Equal(t, mapIDs.previousWindowID, rawIDs.previousWindowID)
		})
	}
}

// null 是合法 JSON，但不是可写元数据对象；保持原文而非触发 nil map panic。
func TestCodexFingerprintNullTurnMetadataDoesNotPanic(t *testing.T) {
	account := newTestOAuthAccount(1204, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	for _, raw := range []string{"null", "[]", "true", `"text"`, "{"} {
		t.Run(raw, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("x-codex-turn-metadata", raw)
			assert.NotPanics(t, func() { applyCodexFingerprintHeaders(headers, ids) })
			assert.Equal(t, raw, headers.Get("x-codex-turn-metadata"))
			body := map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": raw}}
			assert.NotPanics(t, func() { applyCodexFingerprintClientMetadata(body, ids) })
		})
	}
}

func TestCodexFingerprintFirstWindowClearsPrevious(t *testing.T) {
	account := newTestOAuthAccount(1205, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	raw := []byte(`{"client_metadata":{"session_id":"first-window-session","window_number":"0","previous_window_id":"stale","x-codex-turn-metadata":"{\"previous_window_id\":\"stale\"}"}}`)
	ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	require.NotNil(t, ids)
	require.Empty(t, ids.previousWindowID)
	updated, _, err := applyCodexFingerprintClientMetadataRaw(raw, ids)
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(updated, &body))
	metadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, metadata, "previous_window_id")
	embedded, ok := metadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var nested map[string]any
	require.NoError(t, json.Unmarshal([]byte(embedded), &nested))
	assert.NotContains(t, nested, "previous_window_id")
	headers := make(http.Header)
	headers.Set("x-codex-turn-metadata", `{"previous_window_id":"stale"}`)
	applyCodexFingerprintHeaders(headers, ids)
	var headerMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &headerMetadata))
	assert.NotContains(t, headerMetadata, "previous_window_id")
}

func TestCodexFingerprintPre151WindowNumberDoesNotChangeGeneration(t *testing.T) {
	for _, mode := range []string{"session", "cockpit", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(1206, map[string]any{codexFingerprintModeExtraKey: mode})
			headers := make(http.Header)
			headers.Set("User-Agent", "Codex Desktop/0.145.0 (Windows 10; x86_64)")
			body := []byte(`{"client_metadata":{"session_id":"legacy-session","x-codex-window-id":"legacy:0","window_number":3}}`)
			ids := resolveCodexFingerprintIDsFromRawRequest(account, headers, body)
			require.NotNil(t, ids)
			require.False(t, ids.extendedTurnIdentity)
			assert.Equal(t, ids.threadID+":0", ids.windowID, "已剥离的扩展字段不应继续改变旧客户端的窗口与缓存绑定")
		})
	}
}

// Cockpit 只收敛客户端实际携带的回合字段；内部窗口派生不得扩充上游协议形状。
func TestCockpitFingerprintDoesNotSynthesizeAbsentTurnOrWindowNumber(t *testing.T) {
	account := newTestOAuthAccount(1207, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	raw := []byte(`{"prompt_cache_key":"client-cache","client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-thread:2","x-codex-turn-metadata":"{\"extra\":\"keep\"}"}}`)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	ids := resolveCodexFingerprintIDsFromRequest(account, nil, decoded)
	require.NotNil(t, ids)
	require.Empty(t, ids.turnID)
	require.Equal(t, "client-cache", ids.promptCacheKey)
	require.True(t, applyCodexFingerprintClientMetadata(decoded, ids))
	metadata, ok := decoded["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, metadata, "turn_id")
	assert.NotContains(t, metadata, "window_number")
	assert.NotContains(t, metadata, "context_window_id")
	assert.NotContains(t, metadata, "first_window_id")
	assert.NotContains(t, metadata, "previous_window_id")
	assert.Equal(t, "client-cache", decoded["prompt_cache_key"])
	nestedRaw, ok := metadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	nested := gjson.Parse(nestedRaw)
	assert.False(t, nested.Get("turn_id").Exists())
	assert.False(t, nested.Get("window_number").Exists())
	assert.False(t, nested.Get("context_window_id").Exists())
	assert.False(t, nested.Get("first_window_id").Exists())
	assert.False(t, nested.Get("previous_window_id").Exists())
	assert.False(t, nested.Get("prompt_cache_key").Exists(), "官方 Codex 仅在 Responses 顶层发送 prompt_cache_key")
	assert.Equal(t, "keep", nested.Get("extra").String())

	rawIDs := resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
	updated, changed, err := applyCodexFingerprintClientMetadataRaw(raw, rawIDs)
	require.NoError(t, err)
	require.True(t, changed)
	assert.False(t, gjson.GetBytes(updated, "client_metadata.turn_id").Exists())
	assert.False(t, gjson.GetBytes(updated, "client_metadata.window_number").Exists())
	assert.False(t, gjson.GetBytes(updated, "client_metadata.context_window_id").Exists())
	assert.False(t, gjson.GetBytes(updated, "client_metadata.first_window_id").Exists())
	assert.False(t, gjson.GetBytes(updated, "client_metadata.previous_window_id").Exists())
	assert.Equal(t, "client-cache", gjson.GetBytes(updated, "prompt_cache_key").String())

	headers := make(http.Header)
	headers.Set("x-codex-turn-metadata", `{"extra":"keep"}`)
	applyCodexFingerprintHeaders(headers, ids)
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "turn_id").Exists())
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "window_number").Exists())
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "context_window_id").Exists())
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "first_window_id").Exists())
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "previous_window_id").Exists())
	assert.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "prompt_cache_key").Exists())
}

func assertParentTurnMetadataContract(t *testing.T, body map[string]any, parent, root string) {
	t.Helper()
	assert.NotContains(t, body, "parent_turn_id", "Responses 顶层出现 parent_turn_id 会触发 Unsupported parameter")
	assert.NotContains(t, body, "root_turn_id")
	metadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, parent, metadata["parent_turn_id"])
	assert.Equal(t, root, metadata["root_turn_id"])
	assert.Equal(t, "gpt-6-astra", body["model"])
	assert.Equal(t, "parent-contract-input", body["input"])
	assert.Equal(t, "parent-cache", body["prompt_cache_key"])
}
