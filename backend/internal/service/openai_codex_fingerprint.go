package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFingerprintIDsContextKey 保存单次透传尝试的收敛 ID。请求体与请求头必须
// 共享它，才能保证每次请求随机生成的 turn ID 在两个载体中一致。
const codexFingerprintIDsContextKey = "codex_fingerprint_ids"

// stageCodexFingerprintIDs 无条件写入当前尝试的 ID（包括 nil）。故障转移从
// 收敛账号切到关闭收敛的账号时，不能沿用旧账号留在 Gin context 中的值。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
		c.Set(codexFingerprintIDsContextKey, ids)
	}
}

// applyStagedCodexFingerprintHeaders 将透传路径暂存的 ID 应用于出站头。账号
// 类型校验阻止混合账号故障转移时的残留状态影响 API Key 请求。
func applyStagedCodexFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	if c == nil || account == nil || account.Type != AccountTypeOAuth {
		return
	}
	value, ok := c.Get(codexFingerprintIDsContextKey)
	if !ok {
		return
	}
	if ids, ok := value.(*codexFingerprintIDs); ok {
		applyCodexFingerprintHeaders(h, ids)
	}
}

// codexFingerprintMode 控制 OAuth 账号出站请求的设备指纹收敛强度。
// 多人共享同一 OAuth 账号时，每个用户的 Codex 客户端会携带各自不同的
// installation_id / session_id / thread_id，上游据此判定设备数和会话数。
// 收敛模式将这些标识改写为账号级恒定值，减少上游可见的设备/会话指纹。
type codexFingerprintMode string

const (
	// codexFingerprintOff 不做额外收敛，保留现有转发行为。
	codexFingerprintOff codexFingerprintMode = "off"
	// codexFingerprintDevice 仅收敛 installation_id 为账号级恒定值。
	// 上游看到 1 台设备 + 多会话（每用户各自的 session）。
	codexFingerprintDevice codexFingerprintMode = "device"
	// codexFingerprintSession 收敛 installation_id + session_id，
	// thread_id 按客户端原始 session-id 确定性派生（每个真实 Codex 会话一个独立线程）。
	// 上游看到 1 台设备 + 1 会话 + N 线程，最接近正常用户 spawn 子代理的模式。
	codexFingerprintSession codexFingerprintMode = "session"
	// codexFingerprintCockpit 在 session 拓扑基础上兼容 Cockpit 的身份混淆行为：
	// 从请求体补充识别会话种子，并按账号稳定派生 prompt_cache_key。
	codexFingerprintCockpit codexFingerprintMode = "cockpit"
	// codexFingerprintFull 收敛所有标识：installation_id + session_id + thread_id。
	// 上游看到 1 台设备 + 1 会话 + 1 线程，最激进。
	codexFingerprintFull codexFingerprintMode = "full"
)

const codexFingerprintModeExtraKey = "codex_fingerprint_mode"

// GetCodexFingerprintMode 从账号 extra JSON 读取指纹收敛模式。
// 未设置时默认 cockpit（设备+会话+缓存键收敛），显式设为 "off" 才关闭。
func (a *Account) GetCodexFingerprintMode() codexFingerprintMode {
	if a == nil || !a.IsOpenAIOAuth() {
		return codexFingerprintOff
	}
	raw := strings.TrimSpace(a.GetExtraString(codexFingerprintModeExtraKey))
	switch codexFingerprintMode(raw) {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintCockpit, codexFingerprintFull:
		return codexFingerprintMode(raw)
	default:
		return codexFingerprintCockpit
	}
}

// deriveStableUUIDv4 从种子确定性派生一个 UUIDv4 格式的字符串。
// 同一种子永远返回同一值。
func deriveStableUUIDv4(seed string) string {
	h := sha256.Sum256([]byte(seed))
	b := h[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		binary.BigEndian.Uint32(b[0:4]),
		binary.BigEndian.Uint16(b[4:6]),
		binary.BigEndian.Uint16(b[6:8]),
		binary.BigEndian.Uint16(b[8:10]),
		b[10:16])
}

// codexFingerprintStableAccountID 返回用于确定性派生的凭据账号 ID。
// Spark 影子账号与父账号共享同一份 OAuth 凭据，因此必须收敛到父账号的稳定标识；
// 正常转发会先解析父账号，此处仍保留防御性处理，避免其他调用方误用影子 ID。
func codexFingerprintStableAccountID(account *Account) int64 {
	if account == nil {
		return 0
	}
	if account.ParentAccountID != nil && *account.ParentAccountID > 0 {
		return *account.ParentAccountID
	}
	return account.ID
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 优先使用管理员配置的真实 device_id，无则从 accountID 确定性派生。
func resolveConvergedInstallationID(account *Account) string {
	if account == nil {
		return ""
	}
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" {
		return deviceID
	}
	return deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-install-id:v1:%d", codexFingerprintStableAccountID(account)))
}

// resolveConvergedSessionID 返回账号级恒定的 session_id。
func resolveConvergedSessionID(account *Account) string {
	if account == nil {
		return ""
	}
	return deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-session-id:v1:%d", codexFingerprintStableAccountID(account)))
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(account *Account, clientSessionID string) string {
	if account == nil || clientSessionID == "" {
		return ""
	}
	return deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-thread-id:v1:%d:%s", codexFingerprintStableAccountID(account), clientSessionID))
}

// resolveConvergedPromptCacheKey 按账号和客户端原始缓存键稳定派生上游缓存键。
// 相同账号的相同对话保持稳定，不同账号或不同对话互相隔离。
func resolveConvergedPromptCacheKey(account *Account, promptCacheKey string) string {
	if account == nil || strings.TrimSpace(promptCacheKey) == "" {
		return ""
	}
	return deriveStableUUIDv4(fmt.Sprintf("sub2api:codex-prompt-cache-key:v1:%d:%s", codexFingerprintStableAccountID(account), strings.TrimSpace(promptCacheKey)))
}

// codexFingerprintSource 保存客户端原始身份字段，供不同模式选择派生种子。
type codexFingerprintSource struct {
	clientSessionID      string
	threadID             string
	promptCacheKey       string
	promptCacheKeyInBody bool
}

// codexFingerprintIDs 收敛后的完整 ID 集合。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 等随机字段一致。
type codexFingerprintIDs struct {
	mode           codexFingerprintMode
	installationID string
	sessionID      string
	threadID       string
	turnID         string
	windowID       string
	promptCacheKey string
	// promptCacheKeyInBody 区分原请求体字段与仅用于 Header 的兼容缓存键。
	promptCacheKeyInBody bool
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的 session-id 头值（连字符形式），用于 session 模式下
// 的 thread_id 派生——每个真实 Codex 会话得到一个独立线程。
// 返回 nil 表示 off 模式，不需要改写。
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsWithSource(account, codexFingerprintSource{clientSessionID: clientSessionID}, mode)
}

// resolveCodexFingerprintIDsWithSource 使用完整客户端身份来源计算出站 ID 集合。
func resolveCodexFingerprintIDsWithSource(account *Account, source codexFingerprintSource, mode codexFingerprintMode) *codexFingerprintIDs {
	if mode == codexFingerprintOff {
		return nil
	}

	ids := &codexFingerprintIDs{mode: mode}

	ids.installationID = resolveConvergedInstallationID(account)
	if ids.installationID == "" {
		return nil
	}

	switch mode {
	case codexFingerprintDevice:
		return ids

	case codexFingerprintSession:
		ids.sessionID = resolveConvergedSessionID(account)
		ids.threadID = resolveConvergedThreadID(account, source.clientSessionID)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		return ids

	case codexFingerprintCockpit:
		ids.sessionID = resolveConvergedSessionID(account)
		threadSeed := strings.TrimSpace(source.threadID)
		if threadSeed == "" {
			threadSeed = strings.TrimSpace(source.clientSessionID)
		}
		ids.threadID = resolveConvergedThreadID(account, threadSeed)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		ids.promptCacheKey = resolveConvergedPromptCacheKey(account, source.promptCacheKey)
		ids.promptCacheKeyInBody = source.promptCacheKeyInBody
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(account)
		ids.threadID = ids.sessionID
		ids.turnID = uuid.Must(uuid.NewV7()).String()
		ids.windowID = ids.threadID + ":0"
		return ids
	}

	return nil
}

// extractCodexStringField 读取 map 中的非空字符串字段。
func extractCodexStringField(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

// extractCockpitFingerprintSource 按 Cockpit 的兼容顺序从头和请求体提取身份来源。
func extractCockpitFingerprintSource(h http.Header, reqBody map[string]any) codexFingerprintSource {
	source := codexFingerprintSource{clientSessionID: extractClientSessionID(h)}
	clientMetadata, _ := reqBody["client_metadata"].(map[string]any)

	if source.clientSessionID == "" {
		for _, value := range []string{
			extractCodexStringField(reqBody, "session_id"),
			extractCodexStringField(reqBody, "session-id"),
			extractCodexStringField(clientMetadata, "session_id"),
			extractCodexStringField(clientMetadata, "x-codex-window-id"),
			extractCodexStringField(reqBody, "prompt_cache_key"),
		} {
			if value != "" {
				source.clientSessionID = strings.TrimSuffix(value, ":0")
				break
			}
		}
	}

	source.threadID = extractCodexStringField(clientMetadata, "thread_id")
	if source.threadID == "" {
		source.threadID = extractCodexStringField(reqBody, "thread_id")
	}
	source.promptCacheKey = extractCodexStringField(reqBody, "prompt_cache_key")
	source.promptCacheKeyInBody = source.promptCacheKey != ""
	return source
}

// extractCockpitFingerprintSourceRaw 从原始 JSON 中局部读取 Cockpit 身份字段，
// 避免 OAuth 透传路径为大请求体做整包反序列化。
func extractCockpitFingerprintSourceRaw(h http.Header, body []byte) codexFingerprintSource {
	source := codexFingerprintSource{clientSessionID: extractClientSessionID(h)}
	read := func(path string) string {
		return strings.TrimSpace(gjson.GetBytes(body, path).String())
	}

	if source.clientSessionID == "" {
		for _, value := range []string{
			read("session_id"),
			read("session-id"),
			read("client_metadata.session_id"),
			read("client_metadata.x-codex-window-id"),
			read("prompt_cache_key"),
		} {
			if value != "" {
				source.clientSessionID = strings.TrimSuffix(value, ":0")
				break
			}
		}
	}

	source.threadID = read("client_metadata.thread_id")
	if source.threadID == "" {
		source.threadID = read("thread_id")
	}
	source.promptCacheKey = read("prompt_cache_key")
	source.promptCacheKeyInBody = source.promptCacheKey != ""
	return source
}

// extractClientSessionID 从请求头中提取客户端原始的会话标识。
// 优先取 session-id（连字符形式，Codex CLI 标准），回退到 session_id（下划线形式）。
// 返回的值尚未被 isolateOpenAISessionID 改写，是客户端的真实标识。
func extractClientSessionID(h http.Header) string {
	if v := strings.TrimSpace(h.Get("session-id")); v != "" {
		return v
	}
	return strings.TrimSpace(h.Get("session_id"))
}

// resolveCodexFingerprintIDsFromRequest 从客户端原始请求头中提取 session-id，
// 结合账号配置一次性解析收敛 ID 集合。调用方应将返回的 ids 同时传给
// applyCodexFingerprintHeaders 和 applyCodexFingerprintClientMetadata。
func resolveCodexFingerprintIDsFromRequest(account *Account, clientHeaders http.Header, reqBodies ...map[string]any) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	if mode == codexFingerprintCockpit {
		var reqBody map[string]any
		if len(reqBodies) > 0 {
			reqBody = reqBodies[0]
		}
		return resolveCodexFingerprintIDsWithSource(account, extractCockpitFingerprintSource(clientHeaders, reqBody), mode)
	}
	return resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), mode)
}

// resolveCodexFingerprintIDsFromRawRequest 为透传路径从原始请求体提取 Cockpit 会话来源。
func resolveCodexFingerprintIDsFromRawRequest(account *Account, clientHeaders http.Header, body []byte) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	if mode == codexFingerprintCockpit {
		return resolveCodexFingerprintIDsWithSource(account, extractCockpitFingerprintSourceRaw(clientHeaders, body), mode)
	}
	return resolveCodexFingerprintIDs(account, extractClientSessionID(clientHeaders), mode)
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 完成白名单透传和身份头收口后调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}

	// 所有非 off 模式都收敛 installation_id
	h.Set("x-codex-installation-id", ids.installationID)

	if ids.mode == codexFingerprintDevice {
		rewriteCodexTurnMetadataFields(h, map[string]any{
			"installation_id": ids.installationID,
		})
		return
	}

	// session / full 模式：改写所有相关头
	h.Set("x-codex-window-id", ids.windowID)
	h.Set("x-client-request-id", ids.threadID)
	// 连字符形式和下划线形式都改写，保证一致
	h.Set("session-id", ids.sessionID)
	h.Set("session_id", ids.sessionID)
	h.Set("thread-id", ids.threadID)
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && strings.TrimSpace(h.Get("conversation_id")) != "" {
		h.Set("conversation_id", ids.promptCacheKey)
	}

	fields := map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" {
		fields["prompt_cache_key"] = ids.promptCacheKey
	}
	rewriteCodexTurnMetadataFields(h, fields)
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写。保留未指定字段原样（如 sandbox、thread_source 等）。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	raw := strings.TrimSpace(h.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
	}
	for k, v := range fields {
		metadata[k] = v
	}
	rebuilt, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	h.Set("x-codex-turn-metadata", string(rebuilt))
}

// applyCodexFingerprintClientMetadata 按预计算的收敛 ID 改写请求体中的 client_metadata。
// 使用与头改写相同的 ids 实例，确保 turn_id 等随机字段一致。
func applyCodexFingerprintClientMetadata(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil || ids == nil {
		return false
	}

	existing, _ := reqBody["client_metadata"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}
	if !applyCodexFingerprintToClientMetadataMap(existing, ids) {
		return false
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && ids.promptCacheKeyInBody {
		reqBody["prompt_cache_key"] = ids.promptCacheKey
	}
	reqBody["client_metadata"] = existing
	return true
}

// applyCodexFingerprintToClientMetadataMap 是解码体与透传原始 JSON 共用的改写
// 核心，保证两条转发路径不会出现不同的收敛字段语义。
func applyCodexFingerprintToClientMetadataMap(existing map[string]any, ids *codexFingerprintIDs) bool {
	if existing == nil || ids == nil {
		return false
	}

	modified := false

	if ids.installationID != "" {
		existing["x-codex-installation-id"] = ids.installationID
		modified = true
	}

	if ids.mode == codexFingerprintDevice {
		rewriteClientMetadataEmbeddedTurnMetadata(existing, map[string]any{
			"installation_id": ids.installationID,
		})
		return modified
	}

	// session / full 模式
	existing["session_id"] = ids.sessionID
	existing["thread_id"] = ids.threadID
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID

	fields := map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" {
		fields["prompt_cache_key"] = ids.promptCacheKey
	}
	rewriteClientMetadataEmbeddedTurnMetadata(existing, fields)
	return true
}

// applyCodexFingerprintClientMetadataRaw 只抽取并改写 client_metadata 小对象，
// 避免 OAuth 透传路径为大请求体做整包反序列化；其它 JSON 字段保持原样。
func applyCodexFingerprintClientMetadataRaw(body []byte, ids *codexFingerprintIDs) ([]byte, bool, error) {
	if len(body) == 0 || ids == nil {
		return body, false, nil
	}
	if !gjson.ParseBytes(body).IsObject() {
		return body, false, nil
	}

	existing := map[string]any{}
	if metadata := gjson.GetBytes(body, "client_metadata"); metadata.IsObject() {
		if err := json.Unmarshal([]byte(metadata.Raw), &existing); err != nil {
			return body, false, fmt.Errorf("decode client_metadata for fingerprint: %w", err)
		}
	}
	if !applyCodexFingerprintToClientMetadataMap(existing, ids) {
		return body, false, nil
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		return body, false, fmt.Errorf("encode converged client_metadata: %w", err)
	}
	updated, err := sjson.SetRawBytes(body, "client_metadata", raw)
	if err != nil {
		return body, false, fmt.Errorf("splice converged client_metadata: %w", err)
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && ids.promptCacheKeyInBody {
		updated, err = sjson.SetBytes(updated, "prompt_cache_key", ids.promptCacheKey)
		if err != nil {
			return body, false, fmt.Errorf("splice converged prompt_cache_key: %w", err)
		}
	}
	return updated, true, nil
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	raw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	if !ok || raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
	}
	for k, v := range fields {
		metadata[k] = v
	}
	if rebuilt, err := json.Marshal(metadata); err == nil {
		clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
	}
}
