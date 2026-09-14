package service

import "fmt"

// 只追踪本次已准备快照引用的值，不复制整份账号 Extra，也不增加数据库访问。
func codexFingerprintSnapshotValues(snapshots []*codexFingerprintIDs) map[string]struct{} {
	var values map[string]struct{}
	for _, ids := range snapshots {
		if ids == nil {
			continue
		}
		if values == nil {
			values = make(map[string]struct{}, 9)
		}
		for _, value := range []string{
			ids.sessionID, ids.threadID, ids.parentThreadID,
			ids.turnID, ids.parentTurnID, ids.rootTurnID,
			ids.contextWindowID, ids.firstWindowID, ids.previousWindowID,
		} {
			if value != "" {
				values[value] = struct{}{}
			}
		}
	}
	return values
}

// 裁剪循环顺带记录本次引用；无需为少量快照值再扫描整份账号绑定。
type codexFingerprintSnapshotBindings struct {
	values   map[string]struct{}
	bindings map[string]string
}

// 空目标表示绑定被裁剪，或同一个旧值对应多个不同结果；这两类都应停止出站。
func collectCodexFingerprintSnapshotChanges(before map[string]string, selected map[string]any, changes map[string]string) map[string]string {
	for key, oldValue := range before {
		newValue := ""
		if binding, ok := parseCodexIdentityBinding(selected[key]); ok {
			newValue = binding.UUID
		}
		// 也记录相同值，避免另一个绑定把同一旧值映射到不同目标时漏掉歧义。
		if existing, ok := changes[oldValue]; ok && existing != newValue {
			newValue = ""
		}
		if changes == nil {
			changes = make(map[string]string)
		}
		changes[oldValue] = newValue
	}
	return changes
}

// @project-doc docs/interfaces/openai_upstream.md#codex_identity_persistence
// 先准备完整副本，落库成功后才提交；缺省字段标志、客户端缓存键及原始引用保持不变。
func reconcileCodexFingerprintSnapshots(snapshots []*codexFingerprintIDs, selected map[string]string) ([]codexFingerprintIDs, error) {
	changed := false
	for oldValue, newValue := range selected {
		if oldValue != newValue {
			changed = true
			break
		}
	}
	if !changed {
		return nil, nil
	}
	updated := make([]codexFingerprintIDs, len(snapshots))
	for i, ids := range snapshots {
		if ids == nil {
			continue
		}
		// session/thread 是后续回合和窗口派生的依赖。它们发生冲突时不得只换
		// 根 ID 而沿用旧派生图；由调用方用刷新后的账号重试或重新建立 WS。
		for _, value := range []string{ids.sessionID, ids.threadID} {
			if winner, found := selected[value]; found && winner != value {
				return nil, fmt.Errorf("codex conversation binding changed during persistence; retry with refreshed account")
			}
		}
		updated[i] = *ids
		for _, field := range []*string{
			&updated[i].parentThreadID, &updated[i].turnID, &updated[i].parentTurnID, &updated[i].rootTurnID,
			&updated[i].contextWindowID, &updated[i].firstWindowID, &updated[i].previousWindowID,
		} {
			if winner, found := selected[*field]; found {
				if winner == "" {
					return nil, fmt.Errorf("codex snapshot binding is missing or ambiguous after persistence merge")
				}
				*field = winner
			}
		}
	}
	return updated, nil
}

// 快照只在已确认写入成功或与最新持久化状态一致时一次性生效。
func commitCodexFingerprintSnapshots(snapshots []*codexFingerprintIDs, updated []codexFingerprintIDs) {
	for i := range updated {
		if snapshots[i] != nil {
			*snapshots[i] = updated[i]
		}
	}
}
