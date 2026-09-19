package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 仓储读写均深复制 Extra，避免同进程指针共享伪装成已经持久化。
type auditDetachedIdentityRepo struct {
	AccountRepository
	mu     sync.Mutex
	stored *Account
	writes int
}

func auditCloneIdentityAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	cloned := *account
	raw, err := json.Marshal(account.Extra)
	if err != nil {
		panic(err)
	}
	cloned.Extra = nil
	if err := json.Unmarshal(raw, &cloned.Extra); err != nil {
		panic(err)
	}
	return &cloned
}

// 核对审计仓储确实与调用方隔离，杜绝未写数据库但共享指针导致的假通过。
func TestAuditWSRepositoryIsolation(t *testing.T) {
	account := newTestOAuthAccount(1998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	account.Extra["audit_nested"] = map[string]any{"value": "before"}
	repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
	accountNested, ok := account.Extra["audit_nested"].(map[string]any)
	require.True(t, ok)
	accountNested["value"] = "after"
	storedNested, ok := repo.stored.Extra["audit_nested"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "before", storedNested["value"])
	read, err := repo.GetByID(context.Background(), account.ID)
	require.NoError(t, err)
	readNested, ok := read.Extra["audit_nested"].(map[string]any)
	require.True(t, ok)
	readNested["value"] = "reader-change"
	require.Equal(t, "before", storedNested["value"])
}

func (repo *auditDetachedIdentityRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	if repo.stored == nil || repo.stored.ID != id {
		return nil, nil
	}
	return auditCloneIdentityAccount(repo.stored), nil
}

func (repo *auditDetachedIdentityRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	return repo.updateExtraLocked(id, updates)
}

func (repo *auditDetachedIdentityRepo) updateExtraLocked(id int64, updates map[string]any) error {
	if repo.stored == nil || repo.stored.ID != id {
		return fmt.Errorf("synthetic account missing")
	}
	raw, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	var detached map[string]any
	if err := json.Unmarshal(raw, &detached); err != nil {
		return err
	}
	for key, value := range detached {
		repo.stored.Extra[key] = value
	}
	repo.writes++
	return nil
}

// WithCodexIdentityBindings 用互斥区模拟数据库账号行锁，供冷快照和 WS 回归测试走生产原子路径。
func (repo *auditDetachedIdentityRepo) WithCodexIdentityBindings(_ context.Context, id int64, prepare func(latest *Account) (map[string]any, error)) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.stored == nil || repo.stored.ID != id {
		return fmt.Errorf("synthetic account missing")
	}
	updates, err := prepare(auditCloneIdentityAccount(repo.stored))
	if err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	return repo.updateExtraLocked(id, updates)
}

// 冷启动仅清除本合成账号的热缓存，不碰其他测试账号，也不访问真实服务。
func auditDropIdentityHotState(accountID int64) {
	prefix := fmt.Sprintf("%d:", accountID)
	codexIdentityHotCache.Range(func(key, _ any) bool {
		if text, ok := key.(string); ok && strings.HasPrefix(text, prefix) {
			codexIdentityHotCache.Delete(key)
		}
		return true
	})
	codexIdentityPersistedHashes.Delete(fmt.Sprint(accountID))
}

// 使用生产 WS 的 prepare 方法验证客户端回合映射跨冷启动保持不变；
// 所有回合类型都使用本地 ContextV7 生成器并写入回合绑定库。
func TestAuditWSTurnConvergenceAcrossColdStart(t *testing.T) {
	for _, test := range []struct {
		name       string
		nativeTurn bool
		newWindow  bool
	}{
		{name: "native_same_window", nativeTurn: true},
		{name: "string_same_window"},
		{name: "string_next_window", newWindow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := newTestOAuthAccount(1998000+codexSnapshotTestAccountID.Add(1),
				map[string]any{codexFingerprintModeExtraKey: "cockpit", codexTurnModeExtraKey: "converge"})
			t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
			repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
			firstTurn, secondTurn := "audit-legacy-turn-one", "audit-legacy-turn-two"
			if test.nativeTurn {
				firstTurn = "01993000-0000-7000-8000-000000000311"
				secondTurn = "01993000-0000-7000-8000-000000000312"
			}
			window := "01993000-0000-7000-8000-000000000321"
			firstBody := auditAssocBody(t, auditAssocRoot, auditAssocRoot, firstTurn, window, 0, nil)
			first, err := prepareCodexFingerprint(context.Background(), repo, account, func(local *Account) *codexFingerprintIDs {
				return resolveCodexFingerprintIDsFromRawRequest(local, nil, firstBody)
			})
			require.NoError(t, err)
			require.NotNil(t, first)
			state := newCodexWebSocketFingerprintState(account, first, nil, firstBody)
			generation := 0
			if test.newWindow {
				generation = 1
				window = "01993000-0000-7000-8000-000000000322"
			}
			secondBody := auditAssocBody(t, auditAssocRoot, auditAssocRoot, secondTurn, window, generation, nil)
			writesBefore := repo.writes
			second, err := state.prepare(context.Background(), repo, secondBody)
			require.NoError(t, err)
			require.NotNil(t, second)
			require.Equal(t, resolveConvergedCockpitTurnID(account, first.sessionID, secondTurn), second.turnID)
			require.NotEqual(t, first.turnID, second.turnID)
			require.NotEmpty(t, readCodexTurnLineageBindings(repo.stored))
			auditDropIdentityHotState(account.ID)
			cold := auditCloneIdentityAccount(repo.stored)
			restored := resolveCodexFingerprintIDsFromRawRequest(cold, nil, secondBody)
			require.NotNil(t, restored)
			t.Logf("WS_PASSTHROUGH native=%v window_changed=%v new_writes=%d cold_equal=%v",
				test.nativeTurn, test.newWindow, repo.writes-writesBefore, restored.turnID == second.turnID)
			assert.Equal(t, second.turnID, restored.turnID, "冷启动应继续使用同一回合映射")
		})
	}
}
