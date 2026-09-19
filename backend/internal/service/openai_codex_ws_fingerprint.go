package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	coderws "github.com/coder/websocket"
)

// codexWebSocketFingerprintState 记录原始帧的会话边界，不用可能不同的握手别名作比较。
// 客户端切换会话应重连，禁止在同一账号连接里复用另一会话的缓存绑定。
type codexWebSocketFingerprintState struct {
	account         *Account
	current         *codexFingerprintIDs
	clientSessionID string
	clientThreadID  string
}

func newCodexWebSocketFingerprintState(account *Account, ids *codexFingerprintIDs, handshake http.Header, firstBody []byte) *codexWebSocketFingerprintState {
	source := extractCockpitFingerprintSourceRaw(handshake, firstBody)
	return &codexWebSocketFingerprintState{
		account: account, current: ids,
		clientSessionID: source.originalSessionID, clientThreadID: source.threadID,
	}
}

// 帧推进与必要的绑定提交共用取消预算；失败不提交连接状态或泄露锁所有权副本。
func (state *codexWebSocketFingerprintState) prepare(ctx context.Context, repo AccountRepository, body []byte) (*codexFingerprintIDs, error) {
	next := *state
	ids, err := withCodexIdentityPreparation(ctx, state.account, func(ctx context.Context, local *Account) (*codexFingerprintIDs, error) {
		next.account = local
		current, err := next.advance(body)
		if err != nil {
			return nil, NewOpenAIWSClientCloseError(coderws.StatusPolicyViolation, err.Error(), err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// 实验模式的任意新回合也可能新增绑定；默认透传不创建回合绑定。
		// 依据实际变更标记避免每帧全量提交。
		windowChanged := current != nil && state.current != nil &&
			(current.windowID != state.current.windowID || current.contextWindowID != state.current.contextWindowID)
		if current != nil && local != nil && (local.codexIdentityBindingsDirty || windowChanged) {
			if err := persistCodexIdentityBindings(ctx, repo, local, current); err != nil {
				return nil, fmt.Errorf("persist websocket Codex fingerprint bindings: %w", err)
			}
		}
		return current, nil
	})
	if err != nil {
		var closeErr *OpenAIWSClientCloseError
		if errors.As(err, &closeErr) {
			return nil, err
		}
		return nil, NewOpenAIWSClientCloseError(coderws.StatusInternalError, "prepare websocket Codex fingerprint bindings failed", err)
	}
	next.account = state.account
	*state = next
	return ids, nil
}

func (state *codexWebSocketFingerprintState) advance(body []byte) (*codexFingerprintIDs, error) {
	if state.current != nil && state.current.extendedTurnIdentity && state.current.mode != codexFingerprintDevice {
		source := extractCockpitFingerprintSourceRaw(nil, body)
		if (source.originalSessionID != "" && state.clientSessionID != "" && source.originalSessionID != state.clientSessionID) ||
			(source.threadID != "" && state.clientThreadID != "" && source.threadID != state.clientThreadID) {
			return nil, fmt.Errorf("websocket conversation identity changed; reconnect for the new session or thread")
		}
		if source.originalSessionID != "" {
			state.clientSessionID = source.originalSessionID
		}
		if source.threadID != "" {
			state.clientThreadID = source.threadID
		}
	}
	state.current = advanceCodexWebSocketFingerprint(state.account, state.current, body)
	return state.current, nil
}

// advanceCodexWebSocketFingerprint 保持连接的账号/设备/会话/thread 不变，
// 只根据现代客户端当前帧推进回合、压缩窗口与缓存键，握手快照不被原地修改。
// 同一帧的内部重试复用已准备好的结果，不再次生成回合 ID。
func advanceCodexWebSocketFingerprint(account *Account, previous *codexFingerprintIDs, body []byte) *codexFingerprintIDs {
	if previous == nil || account == nil || !previous.extendedTurnIdentity || previous.mode == codexFingerprintDevice {
		return previous
	}
	source := extractCockpitFingerprintSourceRaw(nil, body)
	current := *previous
	current.turnIDPresent = source.turnID != ""
	current.turnStartedAtPresent = source.turnStartedAtPresent
	if source.turnID == "" {
		// 当前帧没有 turn_id 时保持字段缺失；不能把握手或上一帧的
		// 原始回合重新带入出站元数据。
		current.originalTurnID = ""
		current.turnID = ""
	} else if source.turnID != previous.originalTurnID {
		current.originalTurnID = source.turnID
		if current.mode == codexFingerprintCockpit {
			current.turnID = ""
		} else {
			current.turnID = newCodexUUIDv7().String()
		}
	}
	// root/parent 以当前帧为准，即使 turn_id 省略或未变也不回灌上一帧的根。
	current.originalParentThreadID = source.parentThreadID
	current.originalParentTurnID = source.parentTurnID
	current.originalRootTurnID = source.rootTurnID
	if current.mode == codexFingerprintCockpit {
		resolveCockpitTurnLineage(account, &current)
	} else {
		current.parentThreadID = resolveCodexParentThreadID(account, current.mode, current.originalThreadID, current.threadID, current.originalParentThreadID)
		current.parentTurnID = source.parentTurnID
		current.rootTurnID = resolveCodexRootTurnID(source.rootTurnID, source.parentTurnID, current.turnID)
	}
	// Cockpit 的开始时间按当前帧存在性处理；不把前帧或本地缓存时间回灌。
	if current.mode == codexFingerprintCockpit {
		current.turnStartedAtUnixMS = source.turnStartedAtUnixMS
	} else if current.turnID != "" {
		current.turnStartedAtUnixMS = resolveCodexTurnStartedAt(account, current.mode, current.sessionID, current.originalTurnID, current.turnID, source.turnStartedAtUnixMS, source.turnStartedAtPresent)
	}
	current.windowNumberPresent = source.windowNumberPresent
	// 可选窗口字段必须逐帧刷新存在性。若本帧省略而沿用握手快照，
	// Cockpit 写出门控会把上一帧字段错误回灌到当前帧。
	current.originalContextWindowID = source.contextWindowID
	current.originalFirstWindowID = source.firstWindowID
	current.originalPreviousWindowID = source.previousWindowID
	if source.windowID != "" || source.windowNumberPresent || source.contextWindowID != "" ||
		source.firstWindowID != "" || source.previousWindowID != "" {
		windowSource := source
		// 缺省代数保留连接内部位置；字段存在性仍由当前帧独立控制。
		if source.windowID == "" && !source.windowNumberPresent {
			windowSource.windowID = previous.windowID
		}
		windowID := normalizeCodexWindowID(windowSource.windowID, current.threadID)
		if source.windowNumberPresent {
			windowID = fmt.Sprintf("%s:%d", current.threadID, source.windowNumber)
		}
		if source.contextWindowID == "" && windowID == previous.windowID {
			windowSource.contextWindowID = previous.windowInstanceID
		}
		resolveCodexFingerprintWindow(account, windowSource, &current)
		current.originalWindowID = source.windowID
	}
	if current.mode == codexFingerprintCockpit {
		current.promptCacheKeyPresent = source.promptCacheKeyPresent
		if source.promptCacheKeyPresent {
			current.originalPromptCacheKey = source.promptCacheKey
			current.promptCacheKey = source.promptCacheKey
			current.promptCacheKeyInBody = source.promptCacheKeyInBody
			if current.promptCacheKey != "" {
				rememberCodexPromptCacheKey(account, &current, current.promptCacheKey, current.promptCacheKeyInBody)
			}
		} else if codexPromptCacheWindow(&current) != codexPromptCacheWindow(previous) {
			// 与 HTTP 共用窗口绑定：当前窗口优先，缺省时仅继承直接前一窗口。
			current.originalPromptCacheKey = ""
			current.promptCacheKey = resolveOfficialCockpitPromptCacheKey(current.sessionID, "")
			current.promptCacheKeyInBody = false
			if carried, ok := loadCodexPromptCacheKey(account, &current); ok {
				current.promptCacheKey = carried.Key
				current.promptCacheKeyInBody = carried.InBody
			}
		}
	}
	return &current
}
