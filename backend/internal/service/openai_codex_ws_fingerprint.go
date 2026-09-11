package service

import (
	"fmt"
	"net/http"
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
	turnChanged := source.turnID != "" && source.turnID != previous.originalTurnID
	if source.turnID != "" && source.turnID != previous.originalTurnID {
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
	if current.turnID != "" && (turnChanged || current.turnStartedAtUnixMS == 0) {
		current.turnStartedAtUnixMS = resolveCodexTurnStartedAt(account, current.mode, current.sessionID, current.originalTurnID, current.turnID, source.turnStartedAtUnixMS, source.turnStartedAtPresent)
	}
	current.windowNumberPresent = source.windowNumberPresent
	// 可选窗口字段必须逐帧刷新存在性。若本帧省略而沿用握手快照，
	// Cockpit 写出门控会把上一帧字段错误回灌到当前帧。
	current.originalContextWindowID = source.contextWindowID
	current.originalFirstWindowID = source.firstWindowID
	current.originalPreviousWindowID = source.previousWindowID
	if source.windowID != "" || source.windowNumberPresent {
		resolveCodexFingerprintWindow(account, source, &current)
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
		} else if current.windowID != previous.windowID {
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
