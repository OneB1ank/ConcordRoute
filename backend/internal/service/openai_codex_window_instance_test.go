package service

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 窗口实例与代数分别校验，真实 Forward 边界覆盖普通/透传及冷恢复。
func TestCodexWindowInstanceLineageAndRestore(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			account := newTestOAuthAccount(4998000+codexSnapshotTestAccountID.Add(1),
				map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw})
			repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
			var mapped []string
			for i, step := range []struct {
				id, previous string
				number       int
			}{{"W0", "", 0}, {"W1", "W0", 1}, {"W0", "", 0}, {"W2", "W0", 1}, {"W3", "W2", 2}} {
				body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, step.id, step.number,
					map[string]any{"first_window_id": "W0", "previous_window_id": step.previous})
				out := auditAssocForward(t, account, repo, body)
				meta := gjson.GetBytes(out.lastBody, "client_metadata.x-codex-turn-metadata").String()
				id := gjson.Get(meta, "context_window_id").String()
				parsed, err := uuid.Parse(id)
				require.NoError(t, err)
				require.Equal(t, uuid.Version(7), parsed.Version())
				require.Equal(t, uuid.RFC4122, parsed.Variant())
				mapped = append(mapped, id)
				require.Equal(t, mapped[0], gjson.Get(meta, "first_window_id").String())
				if step.previous != "" {
					want := mapped[0]
					if i == 4 {
						want = mapped[3]
					}
					require.Equal(t, want, gjson.Get(meta, "previous_window_id").String())
				}
				require.Equal(t, "unchanged-synthetic-key", gjson.GetBytes(out.lastBody, "prompt_cache_key").String())
				require.Equal(t, int64(step.number), gjson.Get(meta, "window_number").Int())
				auditDropIdentityHotState(account.ID)
				account = auditCloneIdentityAccount(repo.stored)
			}
			require.Equal(t, mapped[0], mapped[2])
			require.NotEqual(t, mapped[1], mapped[3])
			require.NotEqual(t, mapped[3], mapped[4])
		})
	}
}

// 旧代数绑定没有原始 UUID，不据此猜测实例归属；缺少实例时保留旧映射。
func TestCodexWindowInstanceLegacyNamespace(t *testing.T) {
	account := newTestOAuthAccount(4998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	source := codexFingerprintSource{threadID: "thread", clientSessionID: "session", windowID: "thread:1"}
	legacy := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	source.contextWindowID = "W1"
	first := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	source.contextWindowID = "W2"
	second := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.NotEqual(t, legacy.contextWindowID, first.contextWindowID)
	require.NotEqual(t, first.contextWindowID, second.contextWindowID)
	source.contextWindowID = ""
	again := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.Equal(t, legacy.contextWindowID, again.contextWindowID)
	require.Equal(t, legacy.sessionID, first.sessionID)
	require.Equal(t, legacy.threadID, first.threadID)
}

// 实例缓存只继承明确前驱；同代数另一分支、缺失前驱、跨线程/账号均不借键。
func TestCodexWindowInstanceCacheIsolation(t *testing.T) {
	account := newTestOAuthAccount(4998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	source := codexFingerprintSource{threadID: "thread", clientSessionID: "session",
		windowID: "thread:0", contextWindowID: "W0", allowPromptCacheCarry: true,
		promptCacheKey: " root-key ", promptCacheKeyPresent: true, promptCacheKeyInBody: true}
	root := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	source.windowID, source.contextWindowID, source.previousWindowID = "thread:1", "W1", "W0"
	source.promptCacheKey = "branch-one"
	resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	source.contextWindowID, source.promptCacheKey, source.promptCacheKeyPresent = "W2", "", false
	second := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.Equal(t, root.promptCacheKey, second.promptCacheKey)
	source.contextWindowID, source.previousWindowID = "W3", ""
	unknown := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.Equal(t, unknown.sessionID, unknown.promptCacheKey)
	source.contextWindowID, source.previousWindowID = "W4", "W1"
	source.threadID = "other-thread"
	otherThread := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.Equal(t, otherThread.sessionID, otherThread.promptCacheKey)
	otherAccount := newTestOAuthAccount(account.ID+10000, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	source.threadID = "thread"
	other := resolveCodexFingerprintIDsWithSource(otherAccount, source, codexFingerprintCockpit)
	require.Equal(t, other.sessionID, other.promptCacheKey)
	source.contextWindowID, source.promptCacheKeyPresent = "W2", true
	empty := resolveCodexFingerprintIDsWithSource(account, source, codexFingerprintCockpit)
	require.Empty(t, empty.promptCacheKey)
}

// WS 仅 context UUID 改变也必须重算并持久化；省略字段不回灌，键不串分支。
func TestCodexWindowInstanceWebSocket(t *testing.T) {
	account := newTestOAuthAccount(4998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
	body := []byte(`{"prompt_cache_key":"branch-one","client_metadata":{"session_id":"session","thread_id":"thread","context_window_id":"W1","window_number":"1"}}`)
	first, err := prepareCodexFingerprint(context.Background(), repo, account, func(local *Account) *codexFingerprintIDs {
		return resolveCodexFingerprintIDsFromRawRequest(local, nil, body)
	})
	require.NoError(t, err)
	state := newCodexWebSocketFingerprintState(account, first, nil, body)
	body = []byte(`{"client_metadata":{"context_window_id":"W2"}}`)
	next, err := state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.NotEqual(t, first.contextWindowID, next.contextWindowID)
	require.Equal(t, first.windowID, next.windowID)
	require.Equal(t, next.sessionID, next.promptCacheKey)
	auditDropIdentityHotState(account.ID)
	fresh := auditCloneIdentityAccount(repo.stored)
	body, err = sjson.SetBytes(body, "client_metadata.session_id", "session")
	require.NoError(t, err)
	body, err = sjson.SetBytes(body, "client_metadata.thread_id", "thread")
	require.NoError(t, err)
	restored := resolveCodexFingerprintIDsFromRawRequest(fresh, nil, body)
	require.Equal(t, next.contextWindowID, restored.contextWindowID)
	absent, err := state.prepare(context.Background(), repo, []byte(`{"client_metadata":{}}`))
	require.NoError(t, err)
	out, _, err := applyCodexFingerprintClientMetadataRaw([]byte(`{}`), absent)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "client_metadata.context_window_id").Exists())
}

// 同账号多个实例并发准备使用现有锁域；冷恢复后仍对应各自的绑定。
func TestCodexWindowInstanceConcurrentPrepare(t *testing.T) {
	account := newTestOAuthAccount(4998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
	results := make([]*codexFingerprintIDs, 16)
	errs := make([]error, len(results))
	var wg sync.WaitGroup
	for i := range results {
		// 生产 Redis GetAccount 每次解码独立账号；并发共享的是账号锁及存储，不是请求对象。
		requestAccount := auditCloneIdentityAccount(account)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = prepareCodexFingerprint(context.Background(), repo, requestAccount, func(local *Account) *codexFingerprintIDs {
				return resolveCodexFingerprintIDsWithSource(local, codexFingerprintSource{
					clientSessionID: "session", threadID: "thread", windowID: "thread:1",
					contextWindowID: fmt.Sprintf("W%d", i%2),
				}, codexFingerprintCockpit)
			})
		}(i)
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[i%2].contextWindowID, results[i].contextWindowID)
	}
	require.NotEqual(t, results[0].contextWindowID, results[1].contextWindowID)
	auditDropIdentityHotState(account.ID)
	for i := 0; i < 2; i++ {
		fresh := auditCloneIdentityAccount(repo.stored)
		restored := resolveCodexFingerprintIDsWithSource(fresh, codexFingerprintSource{
			clientSessionID: "session", threadID: "thread", windowID: "thread:1",
			contextWindowID: fmt.Sprintf("W%d", i),
		}, codexFingerprintCockpit)
		require.Equal(t, results[i].contextWindowID, restored.contextWindowID)
	}
}
