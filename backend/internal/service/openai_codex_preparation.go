package service

import (
	"context"
	"fmt"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
)

// @project-doc docs/interfaces/openai_upstream.md#codex_identity_persistence
// 一次准备和提交共用预算及账号锁；底层派生复用锁所有权，避免取消前的无界排队。
// 标记只附着于本次同步回调的副本，不污染仓储或调度持有的账号。
func withCodexIdentityPreparation(ctx context.Context, account *Account, prepare func(context.Context, *Account) (*codexFingerprintIDs, error)) (*codexFingerprintIDs, error) {
	ctx, cancel := context.WithTimeout(ctx, codexIdentityPersistenceTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if account == nil {
		return prepare(ctx, nil)
	}
	lock := codexIdentityBindingLock(account.ID)
	finishWait := latencytrace.Start(ctx, "identity_lock_wait")
	err := lock.LockContext(ctx)
	finishWait(err)
	if err != nil {
		return nil, fmt.Errorf("wait for codex identity preparation: %w", err)
	}
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	local := *account
	local.codexIdentityLockHeld = true
	// 派生和持久化可能替换 Extra；所有出口都在锁内同步，不携带锁标记。
	defer func() { account.Extra = local.Extra }()
	ids, err := prepare(ctx, &local)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// 请求入口只负责解析原始来源；生成和持久化的顺序由这里统一维护。
func prepareCodexFingerprint(ctx context.Context, repo AccountRepository, account *Account, resolve func(*Account) *codexFingerprintIDs) (*codexFingerprintIDs, error) {
	// 未启用映射时没有身份生成工作，保留原有持久化与断连后继续收尾的生命周期。
	// 仅真正执行身份准备的路径才新增等锁取消检查。
	if account == nil || account.GetCodexFingerprintMode() == codexFingerprintOff {
		ids := resolve(account)
		return ids, persistCodexIdentityBindings(ctx, repo, account, ids)
	}
	return withCodexIdentityPreparation(ctx, account, func(ctx context.Context, local *Account) (*codexFingerprintIDs, error) {
		ids := resolve(local)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := persistCodexIdentityBindings(ctx, repo, local, ids); err != nil {
			return nil, err
		}
		return ids, nil
	})
}
