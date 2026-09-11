package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 同一会话压缩后暂缺缓存键时，窗口链推进不应把最近的明确缓存键换成 session。
func TestCodexCompactCacheRegressionHTTP(t *testing.T) {
	for _, rawPath := range []bool{false, true} {
		name := "decoded"
		if rawPath {
			name = "raw"
		}
		t.Run(name, func(t *testing.T) {
			account := newTestOAuthAccount(18201, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
			headers := make(http.Header)
			headers.Set("session_id", t.Name())
			resolve := func(body string) *codexFingerprintIDs {
				if rawPath {
					return resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(body))
				}
				var decoded map[string]any
				require.NoError(t, json.Unmarshal([]byte(body), &decoded))
				return resolveCodexFingerprintIDsFromRequest(account, headers, decoded)
			}
			first := resolve(`{"prompt_cache_key":"cache-before-compact","client_metadata":{"window_number":"0"}}`)
			next := resolve(`{"input":[{"type":"compaction","encrypted_content":"synthetic-opaque-content"}],"client_metadata":{"window_number":"1"}}`)
			require.Equal(t, first.sessionID, next.sessionID)
			require.Equal(t, first.threadID, next.threadID)
			require.Equal(t, first.contextWindowID, next.previousWindowID)
			require.Equal(t, first.firstWindowID, next.firstWindowID)
			require.Equal(t, first.promptCacheKey, next.promptCacheKey, "压缩不应隐式切换缓存命名空间")
			require.True(t, next.promptCacheKeyInBody)
		})
	}
}

// 走真实 Forward 的普通转换及透传入口，覆盖旧 compact 的 Header-only 绑定污染。
func TestCodexCompactCacheRegressionForward(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			account := newTestOAuthAccount(18206, map[string]any{
				codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
			})
			account.Credentials = map[string]any{"access_token": "synthetic-test-token"}
			account.Concurrency = 1
			session := uuid.NewString()
			forward := func(path string, generation int, key, headerKey string) (http.Header, []byte) {
				body := map[string]any{
					"model": "gpt-5.1", "instructions": "synthetic instructions",
					"input":           []any{map[string]any{"type": "compaction", "encrypted_content": "synthetic-opaque-content"}},
					"client_metadata": map[string]any{"window_number": fmt.Sprint(generation)},
				}
				if key != "" {
					body["prompt_cache_key"] = key
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				ctx.Request.Header.Set("session_id", session)
				ctx.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
				if headerKey != "" {
					ctx.Request.Header.Set("conversation_id", headerKey)
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic capture stop"}}`)),
				}}
				svc := &OpenAIGatewayService{httpUpstream: upstream}
				_, err = svc.Forward(context.Background(), ctx, account, raw)
				require.Error(t, err, "测试在出站捕获后终止，不访问真实上游")
				require.NotNil(t, upstream.lastReq)
				require.JSONEq(t, gjson.GetBytes(raw, "input").Raw, gjson.GetBytes(upstream.lastBody, "input").Raw)
				return upstream.lastReq.Header, upstream.lastBody
			}
			beforeHeaders, _ := forward("/v1/responses", 0, "body-cache", "")
			_, compactBody := forward("/v1/responses/compact", 0, "", "compact-header-only-cache")
			require.False(t, gjson.GetBytes(compactBody, "prompt_cache_key").Exists())
			require.False(t, gjson.GetBytes(compactBody, "client_metadata").Exists())
			afterHeaders, afterBody := forward("/v1/responses", 1, "", "")
			require.Equal(t, "body-cache", gjson.GetBytes(afterBody, "prompt_cache_key").String())
			require.Equal(t, beforeHeaders.Get("session_id"), afterHeaders.Get("session_id"))
			require.Equal(t, "body-cache", afterHeaders.Get("conversation_id"))
			require.False(t, gjson.GetBytes(afterBody, "client_metadata.root_turn_id").Exists())
		})
	}
}

// 多次压缩、乱序重试、显式换键及账号/线程隔离，均只在合成数据上验证。
func TestCodexCompactCacheRegressionIsolationAndRetry(t *testing.T) {
	account := newTestOAuthAccount(18207, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	session := uuid.NewString()
	resolve := func(account *Account, thread string, generation int, key string) *codexFingerprintIDs {
		body := map[string]any{"client_metadata": map[string]any{
			"session_id": session, "thread_id": thread, "window_number": fmt.Sprint(generation),
		}}
		if key != "" {
			body["prompt_cache_key"] = key
		}
		return resolveCodexFingerprintIDsFromRequest(account, nil, body)
	}
	first := resolve(account, "parent-thread", 0, "cache-A")
	second := resolve(account, "parent-thread", 1, "")
	require.Equal(t, "cache-A", second.promptCacheKey)
	rotated := resolve(account, "parent-thread", 1, "cache-B")
	require.Equal(t, "cache-B", rotated.promptCacheKey)
	resolve(account, "parent-thread", 0, "late-cache-A")
	secondRetry := resolve(account, "parent-thread", 1, "")
	require.Equal(t, "cache-B", secondRetry.promptCacheKey, "旧窗口请求不得覆盖新窗口绑定")
	require.Equal(t, rotated.contextWindowID, secondRetry.contextWindowID)
	third := resolve(account, "parent-thread", 2, "")
	require.Equal(t, "cache-B", third.promptCacheKey)
	require.Equal(t, second.contextWindowID, third.previousWindowID)
	require.Equal(t, first.firstWindowID, third.firstWindowID)
	// 缺了中间窗口时不根据任意更早窗口猜测压缩链。
	require.NotEqual(t, "cache-B", resolve(account, "parent-thread", 4, "").promptCacheKey)
	require.NotEqual(t, "cache-B", resolve(account, "child-thread", 2, "").promptCacheKey)
	otherAccount := newTestOAuthAccount(18208, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	require.NotEqual(t, "cache-B", resolve(otherAccount, "parent-thread", 2, "").promptCacheKey)
	// 新窗口不会反向填回没有历史绑定的首窗口。
	resolve(account, "future-thread", 1, "future-cache")
	require.NotEqual(t, "future-cache", resolve(account, "future-thread", 0, "").promptCacheKey)
	// 过期的前一窗口不复活；仅调整这个测试自己的条目。
	expired := resolve(account, "expired-thread", 0, "expired-cache")
	cacheKey := codexPromptCacheCarryKey(account.ID, expired.sessionID, expired.threadID, expired.windowID)
	codexPromptCacheCarry.Lock()
	entry := codexPromptCacheCarry.items[cacheKey]
	entry.LastUsedAt = time.Now().Add(-codexIdentityBindingIdleTTL - time.Minute).UnixMilli()
	codexPromptCacheCarry.items[cacheKey] = entry
	codexPromptCacheCarry.Unlock()
	require.NotEqual(t, "expired-cache", resolve(account, "expired-thread", 1, "").promptCacheKey)
}

// 同一 WS 上的压缩帧也必须遵守 HTTP 的缓存键继承规则。
func TestCodexCompactCacheRegressionWS(t *testing.T) {
	account := newTestOAuthAccount(18202, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	body := []byte(`{"prompt_cache_key":"ws-before-compact","client_metadata":{"session_id":"compact-regression-ws","window_number":"0"}}`)
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
	state := newCodexWebSocketFingerprintState(account, first, body)
	next, err := state.advance([]byte(`{"client_metadata":{"window_number":"1"}}`))
	require.NoError(t, err)
	require.Equal(t, first.contextWindowID, next.previousWindowID)
	require.Equal(t, first.promptCacheKey, next.promptCacheKey)
	require.True(t, next.promptCacheKeyInBody)
}

// 请求未提供根回合时，解码/局部改写路径以及出站头均保持缺失。
func TestCodexRootAbsentRegression(t *testing.T) {
	for _, mode := range []string{"session", "cockpit", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(18203, map[string]any{codexFingerprintModeExtraKey: mode})
			headers := make(http.Header)
			headers.Set("session_id", t.Name())
			headers.Set("x-codex-turn-metadata", `{"turn_id":"input-turn"}`)
			raw := []byte(`{"client_metadata":{"turn_id":"input-turn","x-codex-turn-metadata":"{\"turn_id\":\"input-turn\"}"}}`)
			ids := resolveCodexFingerprintIDsFromRawRequest(account, headers, raw)
			require.NotNil(t, ids)
			require.Empty(t, ids.rootTurnID, "没有输入根回合就不合成")
			wire, _, err := applyCodexFingerprintClientMetadataRaw(raw, ids)
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(wire, "client_metadata.root_turn_id").Exists())
			require.False(t, gjson.Get(gjson.GetBytes(wire, "client_metadata.x-codex-turn-metadata").String(), "root_turn_id").Exists())
			applyCodexFingerprintHeaders(headers, ids)
			require.False(t, gjson.Get(headers.Get("x-codex-turn-metadata"), "root_turn_id").Exists())
		})
	}
}

// 同一连接的后续帧可以没有回合号；root 的存在性应来自当前帧而非握手快照。
func TestCodexRootAbsentRegressionWS(t *testing.T) {
	for _, metadata := range []string{`{}`, `{"turn_id":"root-turn"}`, `{"turn_id":"new-turn"}`} {
		t.Run(metadata, func(t *testing.T) {
			account := newTestOAuthAccount(18204, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
			first := resolveCodexFingerprintIDsFromRawRequest(account, nil, []byte(`{"client_metadata":{"session_id":"root-frame-session","turn_id":"root-turn","root_turn_id":"supplied-root"}}`))
			require.NotEmpty(t, first.rootTurnID)
			next := advanceCodexWebSocketFingerprint(account, first, []byte(`{"client_metadata":`+metadata+`}`))
			require.Empty(t, next.rootTurnID)
			require.Empty(t, next.originalRootTurnID)
			require.NotEmpty(t, first.rootTurnID, "原握手快照保持不变")
		})
	}
}

// 只有窗口标记时，也应从稳定前缀识别会话，而不是把 :1 纳入 session 的种子。
func TestCodexCompactCacheRegressionWindowOnlySession(t *testing.T) {
	for _, rawPath := range []bool{false, true} {
		t.Run(fmt.Sprint(rawPath), func(t *testing.T) {
			account := newTestOAuthAccount(18205, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
			resolve := func(window int, key string) *codexFingerprintIDs {
				body := map[string]any{"client_metadata": map[string]any{
					"x-codex-window-id": fmt.Sprintf("%s:%d", t.Name(), window),
				}}
				if key != "" {
					body["prompt_cache_key"] = key
				}
				if !rawPath {
					return resolveCodexFingerprintIDsFromRequest(account, nil, body)
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				return resolveCodexFingerprintIDsFromRawRequest(account, nil, raw)
			}
			first := resolve(0, "window-only-cache")
			next := resolve(1, "")
			require.Equal(t, first.sessionID, next.sessionID)
			require.Equal(t, first.threadID, next.threadID)
			require.Equal(t, first.promptCacheKey, next.promptCacheKey)
		})
	}
}

// 显式 root 在没有新 turn_id 的 WS 后续帧中同样有效，且仍只写入元数据。
func TestCodexRootPresentRegressionWS(t *testing.T) {
	account := newTestOAuthAccount(18209, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, []byte(`{"client_metadata":{"session_id":"explicit-root-frame","turn_id":"same-turn"}}`))
	for _, raw := range []string{
		`{"client_metadata":{"root_turn_id":"explicit-root"}}`,
		`{"client_metadata":{"parent_turn_id":"explicit-parent","root_turn_id":"explicit-root"}}`,
	} {
		next := advanceCodexWebSocketFingerprint(account, first, []byte(raw))
		requireCodexUUIDv7(t, next.rootTurnID)
		require.NotEqual(t, "explicit-root", next.rootTurnID)
		if next.parentTurnID != "" {
			requireCodexUUIDv7(t, next.parentTurnID)
			require.NotEqual(t, "explicit-parent", next.parentTurnID)
		} else {
			require.NotEqual(t, next.turnID, next.rootTurnID)
		}
		wire, _, err := applyCodexFingerprintClientMetadataRaw([]byte(raw), next)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(wire, "root_turn_id").Exists())
		require.Equal(t, next.rootTurnID, gjson.GetBytes(wire, "client_metadata.root_turn_id").String())
	}
}

// 旧窗口和新窗口并发绑定时保持独立，覆盖相邻继承使用的共享缓存锁。
func TestCodexCompactCacheRegressionConcurrentWindows(t *testing.T) {
	account := newTestOAuthAccount(18210, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	headers := make(http.Header)
	headers.Set("session_id", uuid.NewString())
	first := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"prompt_cache_key":"old-key"}`))
	next := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"client_metadata":{"window_number":"1"},"prompt_cache_key":"new-key"}`))
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Go(func() {
			for attempt := 0; attempt < 64; attempt++ {
				rememberCodexPromptCacheKey(account, first, "late-old-key", true)
				rememberCodexPromptCacheKey(account, next, "new-key", true)
				entry, ok := loadCodexPromptCacheKey(account, next)
				if !ok || entry.Key != "new-key" {
					t.Errorf("新窗口缓存键被旧窗口污染：ok=%v key=%q", ok, entry.Key)
					return
				}
			}
		})
	}
	workers.Wait()
}

// 显式 Body 键优先于 Header；纯 Header 绑定跨窗口后仍保持原载体。
func TestCodexCompactCacheRegressionKeyCarrier(t *testing.T) {
	account := newTestOAuthAccount(18211, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	headers := make(http.Header)
	headers.Set("session_id", uuid.NewString())
	headers.Set("conversation_id", "header-key")
	first := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{}`))
	require.False(t, first.promptCacheKeyInBody)
	headers.Del("conversation_id")
	second := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"client_metadata":{"window_number":"1"}}`))
	require.Equal(t, "header-key", second.promptCacheKey)
	wire, _, err := applyCodexFingerprintClientMetadataRaw([]byte(`{}`), second)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(wire, "prompt_cache_key").Exists())
	headers.Set("conversation_id", "stale-header-key")
	explicit := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"prompt_cache_key":"body-wins","client_metadata":{"window_number":"1"}}`))
	require.Equal(t, "body-wins", explicit.promptCacheKey)
	require.True(t, explicit.promptCacheKeyInBody)
	headers.Del("conversation_id")
	third := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"client_metadata":{"window_number":"2"}}`))
	require.Equal(t, "body-wins", third.promptCacheKey)
	require.True(t, third.promptCacheKeyInBody)
}

// 继承和显式写入都使用同一个有界存储；持锁隔离测试夹具后恢复原缓存。
func TestCodexCompactCacheRegressionCapacity(t *testing.T) {
	codexPromptCacheCarry.Lock()
	original := codexPromptCacheCarry.items
	defer func() {
		codexPromptCacheCarry.items = original
		codexPromptCacheCarry.Unlock()
	}()
	codexPromptCacheCarry.items = make(map[string]codexPromptCacheCarryEntry)
	for index := 0; index < codexPromptCacheCarryMaxEntries; index++ {
		storeCodexPromptCacheKeyLocked(fmt.Sprint(index), codexPromptCacheCarryEntry{LastUsedAt: int64(index + 1)})
	}
	storeCodexPromptCacheKeyLocked("next-window", codexPromptCacheCarryEntry{LastUsedAt: 2000})
	require.Len(t, codexPromptCacheCarry.items, codexPromptCacheCarryMaxEntries)
	require.NotContains(t, codexPromptCacheCarry.items, "0")
	require.Contains(t, codexPromptCacheCarry.items, "next-window")
}
