package repository

import (
	"context"

	dbaccount "github.com/TokenFlux/TokenRouter/ent/account"
	"github.com/TokenFlux/TokenRouter/internal/service"
)

var _ service.CodexIdentityBindingsReader = (*accountRepository)(nil)

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
