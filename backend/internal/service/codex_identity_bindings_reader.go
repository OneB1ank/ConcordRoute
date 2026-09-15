package service

import "context"

// CodexIdentityBindingsReader 是持久化热路径的窄读取能力。
// 返回 ID 和最新 Extra，不加载凭据、代理、分组；读取失败必须向调用者传播。
// 独立接口保留旧仓储适配器兼容，生产仓储显式实现并由测试验证选中。
type CodexIdentityBindingsReader interface {
	GetCodexIdentityBindings(ctx context.Context, id int64) (*Account, error)
}

// CodexIdentityBindingsTransactor 在数据库账号行锁内执行一次绑定准备。
// 回调收到最新持久化 Extra，并返回需要原子写回的顶层绑定字段；空更新只提交读取事务。
// 该窄接口让多实例首次生成共享同一个权威起点，同时保留轻量测试仓储的旧读写兼容。
type CodexIdentityBindingsTransactor interface {
	WithCodexIdentityBindings(ctx context.Context, id int64, prepare func(latest *Account) (map[string]any, error)) error
}
