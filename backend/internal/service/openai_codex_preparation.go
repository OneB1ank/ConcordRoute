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
	defer func() {
		account.Extra = local.Extra
		account.codexIdentityBindingsDirty = local.codexIdentityBindingsDirty
	}()
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
		if transactor, ok := repo.(CodexIdentityBindingsTransactor); ok {
			return prepareCodexFingerprintInBindingTransaction(ctx, transactor, local, resolve)
		}
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

// prepareCodexFingerprintInBindingTransaction 先在数据库行锁内恢复权威绑定，再生成本次身份图。
// 不同应用实例因此不会各自接受同一逻辑种子的首次 UUID；事务失败时也不会提交出站快照。
func prepareCodexFingerprintInBindingTransaction(ctx context.Context, transactor CodexIdentityBindingsTransactor, account *Account, resolve func(*Account) *codexFingerprintIDs) (*codexFingerprintIDs, error) {
	var ids *codexFingerprintIDs
	var state codexIdentityPersistenceState
	finishTransaction := latencytrace.Start(ctx, "identity_binding_transaction")
	err := transactor.WithCodexIdentityBindings(ctx, account.ID, func(latest *Account) (map[string]any, error) {
		restoreCodexIdentityBindingsBeforeResolve(account, latest)
		ids = resolve(account)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		finishMerge := latencytrace.Start(ctx, "identity_binding_merge")
		var err error
		state, err = buildCodexIdentityPersistenceState(account, latest, ids)
		finishMerge(err)
		if err != nil {
			return nil, err
		}
		if state.matchesDurable {
			return nil, nil
		}
		return state.updates, nil
	})
	finishTransaction(err)
	if err != nil {
		return nil, fmt.Errorf("prepare codex identity bindings transaction: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 成功事务已经核对或写入完整集合；只登记存在性，热路径仍以每次权威读取为准。
	codexIdentityPersistedHashes.Store(fmt.Sprint(account.ID), "")
	commitCodexIdentityPersistenceState(account, []*codexFingerprintIDs{ids}, state)
	return ids, nil
}
