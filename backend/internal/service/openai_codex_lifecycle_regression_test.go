package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type auditLifecycleRepo struct {
	*auditDetachedIdentityRepo
	failWrite bool
	reads     int
}

func (repo *auditLifecycleRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	repo.reads++
	return repo.auditDetachedIdentityRepo.GetByID(ctx, id)
}

func (repo *auditLifecycleRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if repo.failWrite {
		return errors.New("synthetic unavailable storage")
	}
	return repo.auditDetachedIdentityRepo.UpdateExtra(ctx, id, updates)
}

// 同窗口切换客户端 turn 时建立稳定回合绑定；重复帧不重复写入。
func TestAuditWSTurnConvergencePersistsBindings(t *testing.T) {
	account := newTestOAuthAccount(2998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit", codexTurnModeExtraKey: "converge"})
	t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
	repo := &auditLifecycleRepo{auditDetachedIdentityRepo: &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}}
	body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, "first-string", auditAssocChild, 0, nil)
	first, err := prepareCodexFingerprint(context.Background(), repo, account, func(local *Account) *codexFingerprintIDs {
		return resolveCodexFingerprintIDsFromRawRequest(local, nil, body)
	})
	require.NoError(t, err)
	require.False(t, account.codexIdentityBindingsDirty)
	state := newCodexWebSocketFingerprintState(account, first, nil, body)
	body = auditAssocBody(t, auditAssocRoot, auditAssocRoot, "second-string", auditAssocChild, 0, nil)
	reads, writes := repo.reads, repo.writes
	second, err := state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.False(t, account.codexIdentityBindingsDirty)
	require.Equal(t, resolveConvergedCockpitTurnID(account, first.sessionID, "second-string"), second.turnID)
	require.Greater(t, repo.reads, reads)
	require.Greater(t, repo.writes, writes)
	writesAfterSecond := repo.writes
	_, err = state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.Equal(t, writesAfterSecond, repo.writes, "热帧不应反复写同一绑定")
	require.NotEmpty(t, readCodexTurnLineageBindings(repo.stored))
}

// Cockpit 每帧透传本帧开始时间；后续不同值和缺省都不读取旧生命周期缓存。
func TestAuditWSTimestampPassthrough(t *testing.T) {
	account := newTestOAuthAccount(2998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
	repo := &auditLifecycleRepo{auditDetachedIdentityRepo: &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}}
	makeBody := func(value any) []byte {
		return auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 0,
			map[string]any{"turn_started_at_unix_ms": value})
	}
	body := makeBody(nil)
	first, err := prepareCodexFingerprint(context.Background(), repo, account, func(local *Account) *codexFingerprintIDs {
		return resolveCodexFingerprintIDsFromRawRequest(local, nil, body)
	})
	require.NoError(t, err)
	state := newCodexWebSocketFingerprintState(account, first, nil, body)
	const valid = int64(1789100000000)
	for i, testCase := range []struct {
		value   any
		expect  int64
		present bool
	}{{valid, valid, true}, {valid + 999, valid + 999, true}, {nil, 0, false}} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			reads, writes := repo.reads, repo.writes
			next, err := state.prepare(context.Background(), repo, makeBody(testCase.value))
			require.NoError(t, err)
			require.Equal(t, testCase.expect, next.turnStartedAtUnixMS)
			require.Equal(t, testCase.present, next.turnStartedAtPresent)
			require.Equal(t, first.turnID, next.turnID)
			require.Equal(t, reads, repo.reads)
			require.Equal(t, writes, repo.writes)
		})
	}
}

// 回到已有窗口仍保留原有数据库复核，脏标记优化不能跳过窗口切换的快照一致性检查。
func TestAuditWSKnownWindowRechecksStorage(t *testing.T) {
	account := newTestOAuthAccount(2998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
	repo := &auditLifecycleRepo{auditDetachedIdentityRepo: &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}}
	body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 0, nil)
	first, err := prepareCodexFingerprint(context.Background(), repo, account, func(local *Account) *codexFingerprintIDs {
		return resolveCodexFingerprintIDsFromRawRequest(local, nil, body)
	})
	require.NoError(t, err)
	state := newCodexWebSocketFingerprintState(account, first, nil, body)
	nextBody := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 1, nil)
	_, err = state.prepare(context.Background(), repo, nextBody)
	require.NoError(t, err)
	reads := repo.reads
	_, err = state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.Equal(t, reads+1, repo.reads)
}
