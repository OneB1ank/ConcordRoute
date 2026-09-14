package service

import "context"

// CodexIdentityBindingsReader 是持久化热路径的窄读取能力。
// 返回 ID 和最新 Extra，不加载凭据、代理、分组；读取失败必须向调用者传播。
// 独立接口保留旧仓储适配器兼容，生产仓储显式实现并由测试验证选中。
type CodexIdentityBindingsReader interface {
	GetCodexIdentityBindings(ctx context.Context, id int64) (*Account, error)
}
