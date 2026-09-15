package repository

import (
	"context"
	"encoding/json"
	"errors"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	dbaccount "github.com/TokenFlux/TokenRouter/ent/account"
	"github.com/TokenFlux/TokenRouter/internal/service"
)

var _ service.CodexIdentityBindingsReader = (*accountRepository)(nil)
var _ service.CodexIdentityBindingsTransactor = (*accountRepository)(nil)

// GetCodexIdentityBindings 只读取用于合并持久化绑定的账号字段。
// 沿用账号查询的删除与错误语义，不调用会加载代理、分组的 accountsToService。
func (r *accountRepository) GetCodexIdentityBindings(ctx context.Context, id int64) (*service.Account, error) {
	m, err := r.client.Account.Query().
		Where(dbaccount.IDEQ(id)).
		Select(dbaccount.FieldID, dbaccount.FieldExtra).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	return &service.Account{ID: m.ID, Extra: m.Extra}, nil
}

// WithCodexIdentityBindings 在账号行锁内读取、准备并写回 Codex 绑定。
// 锁覆盖服务层的 UUID 派生，保证不同应用实例不会各自接受不同的首次绑定。
func (r *accountRepository) WithCodexIdentityBindings(ctx context.Context, id int64, prepare func(latest *service.Account) (map[string]any, error)) error {
	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	client := r.client
	if tx != nil {
		client = tx.Client()
		defer func() { _ = tx.Rollback() }()
	}

	rows, err := client.QueryContext(ctx, `
		SELECT id, COALESCE(extra, '{}'::jsonb)
		FROM accounts
		WHERE id = $1 AND deleted_at IS NULL
		FOR UPDATE
	`, id)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}

	var accountID int64
	var rawExtra []byte
	if err := rows.Scan(&accountID, &rawExtra); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	extra := make(map[string]any)
	if len(rawExtra) > 0 {
		if err := json.Unmarshal(rawExtra, &extra); err != nil {
			return err
		}
	}
	updates, err := prepare(&service.Account{ID: accountID, Extra: extra})
	if err != nil {
		return err
	}
	wrote := len(updates) > 0
	if wrote {
		discardDeprecatedAccountExtra(updates)
		payload, err := json.Marshal(updates)
		if err != nil {
			return err
		}
		result, err := client.ExecContext(ctx, `
			UPDATE accounts
			SET extra = (COALESCE(extra, '{}'::jsonb)
				- 'upstream_billing_probe'
				- 'upstream_billing_probe_enabled'
				- 'openai_long_context_billing_enabled'
				- 'codex_quota_overdraft_probe') || $1::jsonb,
				updated_at = NOW()
			WHERE id = $2 AND deleted_at IS NULL
		`, string(payload), id)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return service.ErrAccountNotFound
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if wrote && tx != nil {
		// 绑定字段不触发调度 outbox，但要刷新本实例的账号快照，避免后续请求继续拿旧 Extra。
		r.syncSchedulerAccountSnapshot(ctx, id)
	}
	return nil
}
