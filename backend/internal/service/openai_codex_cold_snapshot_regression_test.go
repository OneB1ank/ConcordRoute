package service

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 四格对照只改变账号快照完整性与该合成账号的本机热缓存。
// 证明条件性连续性缺口，不把它直接视为真实慢首字的原因。
func TestCodexIdentityColdSnapshotContinuity(t *testing.T) {
	for _, stale := range []bool{false, true} {
		for _, cold := range []bool{false, true} {
			t.Run(fmt.Sprintf("stale=%v/cold=%v", stale, cold), func(t *testing.T) {
				account := newTestOAuthAccount(8877000000+codexSnapshotTestAccountID.Add(1),
					map[string]any{codexFingerprintModeExtraKey: "cockpit"})
				t.Cleanup(func() { auditDropIdentityHotState(account.ID) })
				originalSnapshot := auditCloneIdentityAccount(account)
				repo := &auditDetachedIdentityRepo{stored: auditCloneIdentityAccount(account)}
				body := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, auditAssocChild, 1, nil)
				resolve := func(local *Account) *codexFingerprintIDs {
					return resolveCodexFingerprintIDsFromRawRequest(local, http.Header{"Version": []string{"0.153.3"}}, body)
				}
				first, err := prepareCodexFingerprint(context.Background(), repo, account, resolve)
				require.NoError(t, err)
				nextAccount := auditCloneIdentityAccount(repo.stored)
				if stale {
					nextAccount = originalSnapshot
				}
				if cold {
					auditDropIdentityHotState(account.ID)
				}
				// 跨过创建毫秒，避免同毫秒的合并平局偶然掩盖问题。
				time.Sleep(3 * time.Millisecond)
				second, err := prepareCodexFingerprint(context.Background(), repo, nextAccount, resolve)
				require.NoError(t, err)
				t.Logf("COLD_SNAPSHOT stale=%t cold=%t same_session=%t same_thread=%t same_window=%t",
					stale, cold, first.sessionID == second.sessionID, first.threadID == second.threadID,
					first.contextWindowID == second.contextWindowID)
				require.Equal(t, first.sessionID, second.sessionID, "同一逻辑会话的已有持久身份应继续使用")
				require.Equal(t, first.threadID, second.threadID)
				require.Equal(t, first.contextWindowID, second.contextWindowID)
			})
		}
	}
}
