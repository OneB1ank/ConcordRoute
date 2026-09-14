package service

// 提交前只收集发生变化且已存在的缓存项，不预热整份持久化集合。
type codexIdentityHotUpdate struct {
	key   string
	value codexIdentityHotBinding
}

func collectCodexIdentityHotUpdate(updates []codexIdentityHotUpdate, key string, binding codexIdentityBinding) []codexIdentityHotUpdate {
	raw, exists := codexIdentityHotCache.Load(key)
	if !exists {
		return updates
	}
	hot, valid := raw.(codexIdentityHotBinding)
	lastUsed := codexIdentityBindingRecency(binding)
	if valid && hot.LastUsedAtMS > lastUsed {
		lastUsed = hot.LastUsedAtMS
	}
	if valid && hot.UUID == binding.UUID && hot.LastUsedAtMS == lastUsed {
		return updates
	}
	return append(updates, codexIdentityHotUpdate{key: key, value: codexIdentityHotBinding{UUID: binding.UUID, LastUsedAtMS: lastUsed}})
}

// @project-doc docs/interfaces/openai_upstream.md#codex_identity_persistence
// 调用方仍持有账号锁；只有提交成功后才发布，失败不污染后续请求的身份。
func publishCodexIdentityHotBindings(updates []codexIdentityHotUpdate) {
	for _, update := range updates {
		codexIdentityHotCache.Store(update.key, update.value)
	}
}
