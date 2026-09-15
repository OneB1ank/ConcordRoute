package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 每个场景使用独立合成账号，避免多次运行时进程缓存污染复现条件。
var codexSnapshotTestAccountID atomic.Int64

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func codexSnapshotTestBody(t *testing.T, turn string, started int64, generation int, cache string) []byte {
	t.Helper()
	metadata := map[string]any{"session_id": "audit-session", "thread_id": "audit-thread", "turn_id": turn, "context_window_id": fmt.Sprintf("audit-window-%d", generation), "window_number": generation}
	if started > 0 {
		metadata["turn_started_at_unix_ms"] = started
	}
	nested, err := json.Marshal(metadata)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "instructions": "synthetic audit", "input": []any{}, "stream": false, "prompt_cache_key": cache, "client_metadata": map[string]any{"session_id": "audit-session", "thread_id": "audit-thread", "turn_id": turn, "x-codex-turn-metadata": string(nested)}})
	require.NoError(t, err)
	return body
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func codexSnapshotTestForward(t *testing.T, account *Account, repo AccountRepository, body []byte) *httpUpstreamRecorder {
	t.Helper()
	account.Credentials = map[string]any{"access_token": "synthetic-test-token"}
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewBufferString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
		},
	}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "codex-tui/0.153.3 (Windows 10.0.26200; x86_64) xterm-256color")
	c.Request.Header.Set("session-id", "audit-session")
	c.Request.Header.Set("thread-id", "audit-thread")
	c.Request.Header.Set("x-codex-turn-metadata", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	return upstream
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func codexSnapshotTestMetadata(t *testing.T, raw []byte, fields map[string]any, flatTime bool) []byte {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	client, valid := body["client_metadata"].(map[string]any)
	require.True(t, valid)
	var nested map[string]any
	metadata, valid := client["x-codex-turn-metadata"].(string)
	require.True(t, valid)
	require.NoError(t, json.Unmarshal([]byte(metadata), &nested))
	for k, v := range fields {
		nested[k] = v
	}
	if flatTime {
		startedAt, valid := nested["turn_started_at_unix_ms"].(float64)
		require.True(t, valid)
		client["turn_started_at_unix_ms"] = fmt.Sprintf("%.0f", startedAt)
	}
	encoded, err := json.Marshal(nested)
	require.NoError(t, err)
	client["x-codex-turn-metadata"] = string(encoded)
	result, err := json.Marshal(body)
	require.NoError(t, err)
	return result
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func TestCodexSnapshotTimestampCarrierAgreement(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			account := newTestOAuthAccount(989000+codexSnapshotTestAccountID.Add(1), map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough})
			repo := &codexIdentityPersistenceRepo{account: account}
			start := time.Now().Add(-time.Hour).UnixMilli()
			for step := 0; step < 2; step++ {
				body := codexSnapshotTestMetadata(t, codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000112", start+int64(step)*1000, 0, "cache-control"), nil, true)
				captured := codexSnapshotTestForward(t, account, repo, body)
				flat := gjson.GetBytes(captured.lastBody, "client_metadata.turn_started_at_unix_ms").Int()
				nested := gjson.Get(gjson.GetBytes(captured.lastBody, "client_metadata.x-codex-turn-metadata").String(), "turn_started_at_unix_ms").Int()
				require.Equal(t, nested, flat, "同一最终请求的平铺与内嵌开始时间应一致")
				require.Equal(t, start+int64(step)*1000, flat)
				require.Equal(t, flat, gjson.Get(captured.lastReq.Header.Get("x-codex-turn-metadata"), "turn_started_at_unix_ms").Int())
			}
		})
	}
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func TestCodexSnapshotDurableWindowMatchesFinalRequest(t *testing.T) {
	codexSnapshotTestDurableWindow(t, true)
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func TestCodexSnapshotDurableWindowNoConflict(t *testing.T) {
	codexSnapshotTestDurableWindow(t, false)
}

// 最终出站与持久化结果的本地回归；仅使用合成数据和 mock 上游。
func codexSnapshotTestDurableWindow(t *testing.T, conflict bool) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			accountID := 989000 + codexSnapshotTestAccountID.Add(1)
			account := newTestOAuthAccount(accountID, map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough})
			body := codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000115", time.Now().UnixMilli(), 0, "cache-control")
			ids := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
			require.NotNil(t, ids)
			encoded, err := json.Marshal(account.Extra)
			require.NoError(t, err)
			var latestExtra map[string]any
			require.NoError(t, json.Unmarshal(encoded, &latestExtra))
			latest := newTestOAuthAccount(account.ID, latestExtra)
			now := time.Now().UnixMilli()
			staleBindings := readCodexIdentityBindings(account)
			latestBindings := readCodexIdentityBindings(latest)
			var changedKey string
			for key, value := range staleBindings {
				binding, ok := parseCodexIdentityBinding(value)
				require.True(t, ok)
				if binding.UUID == ids.contextWindowID {
					binding.CreatedAtMS = now - 2000
					binding.LastUsedAtMS = now - 1000
					staleBindings[key] = binding
					if conflict {
						binding.UUID = newCodexUUIDv7().String()
					}
					binding.LastUsedAtMS = now - 100
					latestBindings[key] = binding
					changedKey = key
					break
				}
			}
			require.NotEmpty(t, changedKey)
			repo := &codexIdentityPersistenceRepo{account: account, latest: latest}
			captured := codexSnapshotTestForward(t, account, repo, body)
			durable, ok := parseCodexIdentityBinding(readCodexIdentityBindings(latest)[changedKey])
			require.True(t, ok)
			finalID := gjson.GetBytes(captured.lastBody, "client_metadata.context_window_id").String()
			require.Equal(t, finalID, gjson.Get(captured.lastReq.Header.Get("x-codex-turn-metadata"), "context_window_id").String())
			require.Equal(t, "cache-control", gjson.GetBytes(captured.lastBody, "prompt_cache_key").String())
			retry := codexSnapshotTestForward(t, account, repo, body)
			retryID := gjson.GetBytes(retry.lastBody, "client_metadata.context_window_id").String()
			require.Equal(t, durable.UUID, retryID, "第二次转发应使用已合并的持久窗口绑定")
			require.Equal(t, captured.lastReq.Header.Get("session-id"), retry.lastReq.Header.Get("session-id"))
			require.Equal(t, captured.lastReq.Header.Get("thread-id"), retry.lastReq.Header.Get("thread-id"))
			t.Logf("conflict=%v first_matches_durable=%v next_matches_durable=%v session/thread_stable=true", conflict, finalID == durable.UUID, retryID == durable.UUID)
			require.Equal(t, durable.UUID, finalID, "合并已完成，但最终请求仍携带合并前的窗口快照")
		})
	}
}

// 写入失败和成功哈希跳过都走真实持久化函数，而不是只测字段替换辅助函数。
type codexSnapshotWriteRepo struct {
	codexIdentityPersistenceRepo
	writeErr error
}

func (r *codexSnapshotWriteRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if r.writeErr != nil {
		return r.writeErr
	}
	return r.codexIdentityPersistenceRepo.UpdateExtra(ctx, id, updates)
}

// 最新仓储值和旧请求快照刻意不同，用来验证提交时机及未涉及字段保持原状。
func codexSnapshotPersistenceFixture(t *testing.T) (*Account, *codexSnapshotWriteRepo, *codexFingerprintIDs, string) {
	t.Helper()
	id := 989000 + codexSnapshotTestAccountID.Add(1)
	now := time.Now().UnixMilli()
	oldID, selectedID := newCodexUUIDv7().String(), newCodexUUIDv7().String()
	current := newTestOAuthAccount(id, map[string]any{
		CodexIdentityBindingsExtraKey: map[string]any{
			"window": codexIdentityBinding{UUID: oldID, CreatedAtMS: now - 2000, LastUsedAtMS: now - 1000},
		},
	})
	latest := newTestOAuthAccount(id, map[string]any{
		CodexIdentityBindingsExtraKey: map[string]any{
			"window": codexIdentityBinding{UUID: selectedID, CreatedAtMS: now - 2000, LastUsedAtMS: now - 100},
		},
	})
	repo := &codexSnapshotWriteRepo{codexIdentityPersistenceRepo: codexIdentityPersistenceRepo{account: current, latest: latest}}
	ids := &codexFingerprintIDs{
		mode: codexFingerprintCockpit, sessionID: "session", threadID: "thread",
		contextWindowID: oldID, firstWindowID: oldID, originalContextWindowID: "client-window",
		originalFirstWindowID: "client-first", windowNumber: 0, windowNumberPresent: false,
		promptCacheKey: " explicit client key ", promptCacheKeyPresent: true, promptCacheKeyInBody: true,
		turnID: "unchanged-turn", turnIDPresent: true, turnStartedAtUnixMS: now - 3000,
	}
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete(fmt.Sprint(id)) })
	return current, repo, ids, selectedID
}

func TestCodexSnapshotPersistenceCommitPaths(t *testing.T) {
	for _, mode := range []string{"write_success", "write_failure", "hash_skip"} {
		t.Run(mode, func(t *testing.T) {
			account, repo, ids, selectedID := codexSnapshotPersistenceFixture(t)
			before := *ids
			if mode == "write_failure" {
				repo.writeErr = errors.New("synthetic write failure")
			}
			if mode == "hash_skip" {
				oldBindings := readCodexIdentityBindings(account)
				require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
				account.Extra[CodexIdentityBindingsExtraKey] = oldBindings
			}
			writesBefore := len(repo.updates)
			err := persistCodexIdentityBindings(context.Background(), repo, account, ids)
			if mode == "write_failure" {
				require.ErrorIs(t, err, repo.writeErr)
				require.Equal(t, before, *ids, "存储失败不得提前提交出站快照")
				return
			}
			require.NoError(t, err)
			want := before
			want.contextWindowID, want.firstWindowID = selectedID, selectedID
			require.Equal(t, want, *ids, "仅更新已合并的绑定值，不改变字段存在性、时间和客户端缓存键")
			if mode == "hash_skip" {
				require.Len(t, repo.updates, writesBefore)
			} else {
				require.Len(t, repo.updates, writesBefore+1)
			}
		})
	}
}

func TestCodexSnapshotRejectsChangedDependencies(t *testing.T) {
	for _, dependency := range []string{"session", "thread"} {
		t.Run(dependency, func(t *testing.T) {
			account, repo, ids, _ := codexSnapshotPersistenceFixture(t)
			if dependency == "session" {
				ids.sessionID = ids.contextWindowID
			} else {
				ids.threadID = ids.contextWindowID
			}
			before := *ids
			err := persistCodexIdentityBindings(context.Background(), repo, account, ids)
			require.ErrorContains(t, err, "conversation binding changed")
			require.Equal(t, before, *ids, "依赖变化时不提交半更新的派生图")
			require.Empty(t, repo.updates)
		})
	}
}

func TestCodexSnapshotRejectsMissingOrAmbiguousBindings(t *testing.T) {
	oldID, newID := newCodexUUIDv7().String(), newCodexUUIDv7().String()
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%v", ambiguous), func(t *testing.T) {
			ids := &codexFingerprintIDs{sessionID: "session", threadID: "thread", contextWindowID: oldID}
			before := *ids
			changes := map[string]string{oldID: ""}
			if ambiguous {
				changes[oldID] = newID
				// 第二个绑定保留同一个旧值，使选择结果出现歧义。
				changes = collectCodexFingerprintSnapshotChanges(
					map[string]string{"same-value": oldID},
					map[string]any{"same-value": codexIdentityBinding{UUID: oldID}},
					changes,
				)
			}
			_, err := reconcileCodexFingerprintSnapshots([]*codexFingerprintIDs{ids}, changes)
			require.Error(t, err)
			require.Equal(t, before, *ids)
		})
	}
}

// 平铺时间只有已有有效值才同步；缺省和异常输入继续保持两条路径的一致行为。
func TestCodexSnapshotTimestampPresenceAndTypes(t *testing.T) {
	for _, value := range []string{`"2000"`, `2000`, `null`, `true`, `{}`, `""`} {
		for _, rawPath := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/raw=%v", value, rawPath), func(t *testing.T) {
				ids := &codexFingerprintIDs{mode: codexFingerprintCockpit, turnID: "turn", turnIDPresent: true, turnStartedAtUnixMS: 1000, turnStartedAtPresent: true}
				body := []byte(`{"client_metadata":{"turn_started_at_unix_ms":` + value + `,"x-codex-turn-metadata":"{}"}}`)
				var result []byte
				if rawPath {
					var err error
					result, _, err = applyCodexFingerprintClientMetadataRaw(body, ids)
					require.NoError(t, err)
				} else {
					var decoded map[string]any
					require.NoError(t, json.Unmarshal(body, &decoded))
					applyCodexFingerprintClientMetadata(decoded, ids)
					var err error
					result, err = json.Marshal(decoded)
					require.NoError(t, err)
				}
				expected := value
				switch value {
				case `"2000"`:
					expected = `"1000"`
				case `2000`:
					expected = `1000`
				}
				require.JSONEq(t, expected, gjson.GetBytes(result, "client_metadata.turn_started_at_unix_ms").Raw)
			})
		}
	}
	ids := &codexFingerprintIDs{mode: codexFingerprintCockpit, turnID: "turn", turnIDPresent: true, turnStartedAtUnixMS: 1000, turnStartedAtPresent: true}
	decoded := map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": "{}"}}
	applyCodexFingerprintClientMetadata(decoded, ids)
	client, valid := decoded["client_metadata"].(map[string]any)
	require.True(t, valid)
	_, exists := client["turn_started_at_unix_ms"]
	require.False(t, exists, "缺省平铺时间不补造")
}

func TestCodexSnapshotWebSocketTimestampCarriers(t *testing.T) {
	account := newTestOAuthAccount(989000+codexSnapshotTestAccountID.Add(1), map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	body := codexSnapshotTestMetadata(t, codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000211", 1000, 0, "ws-key"), nil, true)
	first := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
	state := newCodexWebSocketFingerprintState(account, first, nil, body)
	next := codexSnapshotTestMetadata(t, codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000211", 2000, 1, "ws-key-new"), nil, true)
	current, err := state.advance(next)
	require.NoError(t, err)
	result, _, err := applyCodexFingerprintClientMetadataRaw(next, current)
	require.NoError(t, err)
	require.EqualValues(t, 2000, gjson.GetBytes(result, "client_metadata.turn_started_at_unix_ms").Int())
	require.EqualValues(t, 2000, gjson.Get(gjson.GetBytes(result, "client_metadata.x-codex-turn-metadata").String(), "turn_started_at_unix_ms").Int())
	require.Equal(t, first.turnID, current.turnID)
	require.Equal(t, "ws-key-new", gjson.GetBytes(result, "prompt_cache_key").String())
}

// 只比较本地热路径 CPU/分配，不把 mock 的耗时当作生产数据库耗时。
func BenchmarkCodexSnapshotPersistence(b *testing.B) {
	for _, size := range []int{8, 1024} {
		for _, withSnapshot := range []bool{false, true} {
			b.Run(fmt.Sprintf("bindings=%d/snapshot=%v", size, withSnapshot), func(b *testing.B) {
				now := time.Now().UnixMilli()
				bindings := make(map[string]any, size)
				last := ""
				for i := 0; i < size; i++ {
					last = newCodexUUIDv7().String()
					bindings[fmt.Sprint(i)] = codexIdentityBinding{UUID: last, CreatedAtMS: now, LastUsedAtMS: now}
				}
				account := newTestOAuthAccount(989000+codexSnapshotTestAccountID.Add(1), map[string]any{CodexIdentityBindingsExtraKey: bindings})
				repo := &codexIdentityPersistenceRepo{account: account}
				var snapshots []*codexFingerprintIDs
				if withSnapshot {
					snapshots = []*codexFingerprintIDs{{sessionID: "session", threadID: "thread", contextWindowID: last, firstWindowID: last}}
				}
				if err := persistCodexIdentityBindings(context.Background(), repo, account, snapshots...); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := persistCodexIdentityBindings(context.Background(), repo, account, snapshots...); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
