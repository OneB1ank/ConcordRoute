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

// 同窗口新字符串 turn 失败后，重试仍须提交，且只在成功后推进连接状态。
func TestAuditWSFailedTurnCommitRetried(t *testing.T) {
	account := newTestOAuthAccount(2998000+codexSnapshotTestAccountID.Add(1),
		map[string]any{codexFingerprintModeExtraKey: "cockpit"})
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
	repo.failWrite = true
	_, err = state.prepare(context.Background(), repo, body)
	require.Error(t, err)
	require.Same(t, first, state.current)
	require.True(t, account.codexIdentityBindingsDirty)
	repo.failWrite = false
	second, err := state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.False(t, account.codexIdentityBindingsDirty)
	require.True(t, auditDurableContainsTurn(repo.stored, second.turnID))
	reads, writes := repo.reads, repo.writes
	_, err = state.prepare(context.Background(), repo, body)
	require.NoError(t, err)
	require.Equal(t, reads, repo.reads, "热帧不应增加数据库读取")
	require.Equal(t, writes, repo.writes, "热帧不应反复写同一绑定")
}

// 同 turn 的兜底、首次有效值、后续不同值、再缺省必须按同一生命周期处理。
func TestAuditWSFirstValidTimestampLifecycle(t *testing.T) {
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
	for i, value := range []any{valid, valid + 999, nil} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			reads, writes := repo.reads, repo.writes
			next, err := state.prepare(context.Background(), repo, makeBody(value))
			require.NoError(t, err)
			require.Equal(t, valid, next.turnStartedAtUnixMS)
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
