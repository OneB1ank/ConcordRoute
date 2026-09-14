package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 写入与成功哈希跳过均发布选中值；失败不得发布未提交值或污染其它账号/绑定。
func TestCodexSnapshotHotCacheCommitPaths(t *testing.T) {
	for _, store := range []string{CodexIdentityBindingsExtraKey, CodexTurnLineageBindingsExtraKey} {
		for _, mode := range []string{"write_success", "write_failure", "hash_skip"} {
			t.Run(store+"/"+mode, func(t *testing.T) {
				account, repo, ids, selected := codexSnapshotPersistenceFixture(t)
				if store != CodexIdentityBindingsExtraKey {
					account.Extra[store] = account.Extra[CodexIdentityBindingsExtraKey]
					delete(account.Extra, CodexIdentityBindingsExtraKey)
					repo.latest.Extra[store] = repo.latest.Extra[CodexIdentityBindingsExtraKey]
					delete(repo.latest.Extra, CodexIdentityBindingsExtraKey)
				}
				hotKey := fmt.Sprintf("%d:window", account.ID)
				oldHot := codexIdentityHotBinding{UUID: ids.contextWindowID, LastUsedAtMS: time.Now().UnixMilli()}
				otherKey := fmt.Sprintf("%d:window", account.ID+1000000)
				unrelatedKey := fmt.Sprintf("%d:unrelated", account.ID)
				for _, key := range []string{hotKey, otherKey, unrelatedKey} {
					codexIdentityHotCache.Store(key, oldHot)
					t.Cleanup(func() { codexIdentityHotCache.Delete(key) })
				}
				if mode == "hash_skip" {
					oldBindings := readCodexUUIDv7Bindings(account, store)
					require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
					account.Extra[store] = oldBindings
					codexIdentityHotCache.Store(hotKey, oldHot)
				}
				if mode == "write_failure" {
					repo.writeErr = errors.New("synthetic hot-cache publish failure")
				}
				writes := len(repo.updates)
				err := persistCodexIdentityBindings(context.Background(), repo, account, ids)
				hot, exists := codexIdentityHotCache.Load(hotKey)
				require.True(t, exists)
				if mode == "write_failure" {
					require.ErrorIs(t, err, repo.writeErr)
					require.Equal(t, oldHot, hot, "写入失败时不发布选中值")
				} else {
					require.NoError(t, err)
					binding, valid := hot.(codexIdentityHotBinding)
					require.True(t, valid)
					require.Equal(t, selected, binding.UUID)
					require.Equal(t, selected, ids.contextWindowID)
					if mode == "hash_skip" {
						require.Len(t, repo.updates, writes)
					} else {
						require.Len(t, repo.updates, writes+1)
					}
				}
				for _, key := range []string{otherKey, unrelatedKey} {
					value, ok := codexIdentityHotCache.Load(key)
					require.True(t, ok)
					require.Equal(t, oldHot, value, "其它账号或未参与提交的绑定保持不变")
				}
			})
		}
	}
}

// 复核只使用合成账号和内存上游，覆盖调度快照尚未包含运行态绑定的情况。
func codexHotCacheCloneAccount(t *testing.T, account *Account) *Account {
	t.Helper()
	raw, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	var extra map[string]any
	require.NoError(t, json.Unmarshal(raw, &extra))
	return newTestOAuthAccount(account.ID, extra)
}

// 一笔请求已选中数据库 B 后，下一笔持有早期空绑定快照时也应继续使用 B。
func TestCodexSnapshotHotCacheAfterReconcile(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stale := range []bool{false, true} {
			t.Run(fmt.Sprintf("raw=%v/stale=%v", passthrough, stale), func(t *testing.T) {
				account := newTestOAuthAccount(989000+codexSnapshotTestAccountID.Add(1), map[string]any{
					codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
				})
				early := codexHotCacheCloneAccount(t, account)
				body := codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000991", time.Now().UnixMilli(), 0, "review-key")
				prepared := resolveCodexFingerprintIDsFromRawRequest(account, nil, body)
				require.NotNil(t, prepared)
				old := prepared.contextWindowID
				durable := codexHotCacheCloneAccount(t, account)
				selected := newCodexUUIDv7().String()
				var windowKey string
				for key, raw := range readCodexIdentityBindings(account) {
					binding, ok := parseCodexIdentityBinding(raw)
					require.True(t, ok)
					if binding.UUID != old {
						continue
					}
					windowKey = key
					binding.CreatedAtMS = time.Now().Add(-2 * time.Second).UnixMilli()
					binding.LastUsedAtMS = time.Now().Add(-time.Second).UnixMilli()
					readCodexIdentityBindings(account)[key] = binding
					binding.UUID = selected
					binding.LastUsedAtMS = time.Now().Add(-100 * time.Millisecond).UnixMilli()
					readCodexIdentityBindings(durable)[key] = binding
					break
				}
				require.NotEmpty(t, windowKey)
				repo := &codexIdentityPersistenceRepo{account: account, latest: durable}
				first := codexSnapshotTestForward(t, account, repo, body)
				require.Equal(t, selected, gjson.GetBytes(first.lastBody, "client_metadata.context_window_id").String())
				// 观测真实窗口种子的热缓存，区分出站快照提交与缓存提交这两个层次。
				hotKey := codexIdentityHotKey(account, fmt.Sprintf("codex-context-window:%s:0", prepared.threadID))
				hotValue, hotExists := codexIdentityHotCache.Load(hotKey)
				require.True(t, hotExists)
				hotBinding, hotValid := hotValue.(codexIdentityHotBinding)
				require.True(t, hotValid)
				t.Logf("after_first_commit hot_is_A=%v hot_is_B=%v", hotBinding.UUID == old, hotBinding.UUID == selected)
				nextAccount := codexHotCacheCloneAccount(t, durable)
				if stale {
					nextAccount = early
				}
				repo.account = nextAccount
				next := codexSnapshotTestForward(t, nextAccount, repo, body)
				final := gjson.GetBytes(next.lastBody, "client_metadata.context_window_id").String()
				stored, ok := parseCodexIdentityBinding(readCodexIdentityBindings(durable)[windowKey])
				require.True(t, ok)
				require.Equal(t, first.lastReq.Header.Get("session-id"), next.lastReq.Header.Get("session-id"))
				require.Equal(t, first.lastReq.Header.Get("thread-id"), next.lastReq.Header.Get("thread-id"))
				require.Equal(t, "review-key", gjson.GetBytes(next.lastBody, "prompt_cache_key").String())
				t.Logf("stale=%v first_is_B=true second_is_B=%v second_reverted_to_A=%v stored_matches_second=%v", stale, final == selected, final == old, final == stored.UUID)
				require.Equal(t, selected, final, "成功采用数据库 B 后，早期调度快照不应通过热缓存把窗口重置回 A")
			})
		}
	}
}
