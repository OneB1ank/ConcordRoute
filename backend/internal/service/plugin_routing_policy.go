package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

type pluginPrincipalKey struct{}
type pluginPrincipal struct{ userID, groupID int64 }

// WithPluginPrincipal freezes the authenticated principal before scheduler
// fallback/composite routing can change the working group.
func WithPluginPrincipal(ctx context.Context, userID, groupID int64) context.Context {
	return context.WithValue(ctx, pluginPrincipalKey{}, pluginPrincipal{userID: userID, groupID: groupID})
}
func pluginPrincipalFromContext(ctx context.Context) pluginPrincipal {
	if ctx == nil {
		return pluginPrincipal{}
	}
	principal, _ := ctx.Value(pluginPrincipalKey{}).(pluginPrincipal)
	return principal
}
func pluginScopeContains(ids []int64, id int64) bool {
	if len(ids) == 0 {
		return true
	}
	if id <= 0 {
		return false
	}
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}
func pluginScopeEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func normalizePluginScope(ids []int64) ([]int64, error) {
	if len(ids) > 1000 {
		return nil, errors.New("每种插件作用域最多包含 1000 个 ID")
	}
	out := append([]int64{}, ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	for i, id := range out {
		if id <= 0 {
			return nil, errors.New("插件作用域 ID 必须为正整数")
		}
		if i > 0 && out[i-1] == id {
			return nil, errors.New("插件作用域 ID 不能重复")
		}
	}
	return out, nil
}
func (b PluginBinding) EffectiveConcurrency() int {
	if b.MaxConcurrency == 0 {
		return extensionConcurrencyLimit
	}
	return b.MaxConcurrency
}
func (b PluginBinding) Timeout(capability PluginCapability) time.Duration {
	if b.TimeoutMS > 0 && b.TimeoutMS < capability.TimeoutMS {
		return time.Duration(b.TimeoutMS) * time.Millisecond
	}
	return capability.ExtensionCapability().TimeoutDuration()
}

type PluginRoutingPolicy struct {
	Capability     string  `json:"capability"`
	Priority       int     `json:"priority"`
	AccountIDs     []int64 `json:"account_ids"`
	UserIDs        []int64 `json:"user_ids"`
	GroupIDs       []int64 `json:"group_ids"`
	RolloutPercent int     `json:"rollout_percent"`
	MaxConcurrency int     `json:"max_concurrency"`
	TimeoutMS      int64   `json:"timeout_ms"`
}
type PluginRoutingRepository interface {
	UpdateRouting(context.Context, *PluginInstallation, []PluginBinding) (*PluginInstallation, error)
}

func (m *PluginManager) SaveRouting(ctx context.Context, id int64, policies []PluginRoutingPolicy, expectedUpdatedAt time.Time) (*PluginInstallation, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	repo, ok := m.repo.(PluginRoutingRepository)
	if !ok {
		return nil, errors.New("插件路由存储不可用")
	}
	current, err := m.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if expectedUpdatedAt.IsZero() || !current.UpdatedAt.Equal(expectedUpdatedAt) {
		return nil, ErrPluginStateChanged
	}
	if current.Manifest.SchemaVersion != 2 {
		return nil, errors.New("细粒度路由仅适用于 v2 扩展插件")
	}
	if current.State == PluginStateStarting {
		return nil, ErrPluginStateChanged
	}
	if len(policies) != len(current.Bindings) || len(policies) == 0 {
		return nil, errors.New("必须提交每个插件能力的路由策略")
	}
	bindings := append([]PluginBinding(nil), current.Bindings...)
	seen := map[string]bool{}
	for _, policy := range policies {
		if seen[policy.Capability] {
			return nil, errors.New("路由能力重复")
		}
		seen[policy.Capability] = true
		if policy.Priority < -1000 || policy.Priority > 1000 || policy.RolloutPercent < 0 || policy.RolloutPercent > 100 ||
			policy.MaxConcurrency < 1 || policy.MaxConcurrency > 256 || policy.TimeoutMS < 0 || policy.TimeoutMS > 5000 {
			return nil, errors.New("优先级、灰度、并发或超时超出允许范围")
		}
		var cap *PluginCapability
		for i := range current.Manifest.Capabilities {
			if current.Manifest.Capabilities[i].ID == policy.Capability {
				cap = &current.Manifest.Capabilities[i]
				break
			}
		}
		if cap == nil {
			return nil, fmt.Errorf("未声明的能力 %s", policy.Capability)
		}
		if err := supportedExtensionCapability(*cap); err != nil {
			return nil, err
		}
		if policy.TimeoutMS > cap.TimeoutMS {
			return nil, errors.New("路由超时不能超过清单声明")
		}
		accounts, err := normalizePluginScope(policy.AccountIDs)
		if err != nil {
			return nil, err
		}
		users, err := normalizePluginScope(policy.UserIDs)
		if err != nil {
			return nil, err
		}
		groups, err := normalizePluginScope(policy.GroupIDs)
		if err != nil {
			return nil, err
		}
		matched := false
		for i := range bindings {
			if bindings[i].Capability != policy.Capability {
				continue
			}
			b := &bindings[i]
			if b.Platform != cap.Platform || b.AccountType != cap.AccountType {
				return nil, errors.New("插件绑定与清单作用域不一致")
			}
			b.Priority, b.RolloutPercent, b.MaxConcurrency, b.TimeoutMS = policy.Priority, policy.RolloutPercent, policy.MaxConcurrency, policy.TimeoutMS
			b.AccountIDs, b.UserIDs, b.GroupIDs = accounts, users, groups
			matched = true
		}
		if !matched {
			return nil, errors.New("能力绑定不存在")
		}
	}
	saved, err := repo.UpdateRouting(ctx, current, bindings)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	process := m.runtimes[id]
	if process != nil && process.installation.BinarySHA256 == saved.BinarySHA256 {
		process.installation.Bindings = append([]PluginBinding(nil), saved.Bindings...)
		m.publishExtensionRoutesLocked(saved, process, "")
	} else if hasEnabledPluginBinding(saved.Bindings) {
		m.publishExtensionRoutesLocked(saved, nil, "插件实例尚未就绪")
	}
	m.mu.Unlock()
	saved.Compatibility = EvaluatePluginCompatibility(saved.Manifest, m.hostInfo)
	saved.RuntimeHealthy = process != nil && process.client != nil && !process.client.Exited()
	if saved.RuntimeHealthy {
		saved.RuntimeVersion = process.installation.Version
		saved.RuntimeIsolation = process.isolation
	}
	saved.CapabilityRuntime = m.extensionStatus(id)
	return saved, nil
}
