package service

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/semaphore"
)

// codexFingerprintIDsContextKey 保存单次转发尝试的身份快照。请求体与请求头
// 共享回合策略及其结果，避免一个载体透传、另一个载体映射。
const codexFingerprintIDsContextKey = "codex_fingerprint_ids"

// stageCodexFingerprintIDs 无条件写入当前尝试的 ID（包括 nil）。故障转移从
// 收敛账号切到关闭收敛的账号时，不能沿用旧账号留在 Gin context 中的值。
func stageCodexFingerprintIDs(c *gin.Context, ids *codexFingerprintIDs) {
	if c != nil {
		c.Set(codexFingerprintIDsContextKey, ids)
	}
}

// stagedCodexFingerprintIDs 返回当前账号尝试的指纹映射。每次 Forward 开始时会先清空，
// 因此故障转移到另一账号时不会把上一账号的反向映射带入响应。
func stagedCodexFingerprintIDs(c *gin.Context) *codexFingerprintIDs {
	if c == nil {
		return nil
	}
	value, ok := c.Get(codexFingerprintIDsContextKey)
	if !ok {
		return nil
	}
	ids, _ := value.(*codexFingerprintIDs)
	return ids
}

// applyStagedCodexFingerprintHeaders 将透传路径暂存的 ID 应用于出站头。账号
// 类型校验阻止混合账号故障转移时的残留状态影响 API Key 请求。
func applyStagedCodexFingerprintHeaders(c *gin.Context, account *Account, h http.Header) {
	if c == nil || account == nil || account.Type != AccountTypeOAuth {
		return
	}
	if ids := stagedCodexFingerprintIDs(c); ids != nil && ids.stagedAccountBound && ids.stagedAccountID == account.ID {
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
	// codexFingerprintCockpit 固定账号级 installation，并从请求体识别对话种子，
	// 为每个对话稳定派生 session/thread/prompt_cache_key。
	codexFingerprintCockpit codexFingerprintMode = "cockpit"
	// codexFingerprintFull 收敛所有标识：installation_id + session_id + thread_id。
	// 上游看到 1 台设备 + 1 会话 + 1 线程，最激进。
	codexFingerprintFull codexFingerprintMode = "full"
)

const codexFingerprintModeExtraKey = "codex_fingerprint_mode"

// codexTurnMode 独立控制 Cockpit 回合字段；缺省只透传客户端标识。
type codexTurnMode string

const (
	codexTurnModeExtraKey               = "codex_turn_mode"
	codexTurnPassthrough  codexTurnMode = "passthrough"
	codexTurnConverge     codexTurnMode = "converge"
)

func (a *Account) GetCodexTurnMode() codexTurnMode {
	if a != nil && a.IsOpenAIOAuth() && a.Extra != nil {
		if mode, ok := a.Extra[codexTurnModeExtraKey].(string); ok && mode == string(codexTurnConverge) {
			return codexTurnConverge
		}
	}
	return codexTurnPassthrough
}

// codexExtendedTurnIdentityMinVersion is the first Codex engine version whose
// wire metadata includes context_window_id, parent_turn_id and root_turn_id.
// Older clients keep the 0.145-era metadata shape.
const codexExtendedTurnIdentityMinVersion = "0.151.0"

func codexSupportsExtendedTurnIdentity(version string) bool {
	version = NormalizeCodexClientVersion(version)
	// An absent version is common on compatibility/probe paths. Preserve the
	// existing behavior there; an explicitly older client is gated off.
	return version == "" || CompareVersions(version, codexExtendedTurnIdentityMinVersion) >= 0
}

func codexClientVersionFromHeaders(h http.Header) string {
	if h == nil {
		return ""
	}
	versionHeader := NormalizeCodexClientVersion(h.Get("version"))
	uaVersion := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(h.Get("User-Agent")))
	if versionHeader != "" && uaVersion != "" {
		// A mixed request must use the older declaration for capability gating;
		// this prevents a stale UA from receiving fields it does not understand.
		if CompareVersions(versionHeader, uaVersion) <= 0 {
			return versionHeader
		}
		return uaVersion
	}
	if versionHeader != "" {
		return versionHeader
	}
	return uaVersion
}

// CodexFingerprintSeedExtraKey 保存账号级随机指纹种子。种子随账号持久化，
// 避免不同部署中相同的本地自增账号 ID 派生出相同的设备和会话标识。
const CodexFingerprintSeedExtraKey = "codex_fingerprint_seed"

// normalizeCodexFingerprintSeed 校验系统持久化的账号种子并统一为 canonical UUID。
// nil UUID 不能作为有效身份，否则多个损坏账号会再次收敛到同一个固定值。
func normalizeCodexFingerprintSeed(raw string) (string, bool) {
	parsed, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == uuid.Nil {
		return "", false
	}
	return parsed.String(), true
}

// CanonicalCodexFingerprintSeed 向仓储层暴露持久化校验，
// 供持有账号行锁时保持种子归属关系。
func CanonicalCodexFingerprintSeed(raw string) (string, bool) {
	return normalizeCodexFingerprintSeed(raw)
}

// ShouldEnsureCodexFingerprintSeedForExtraUpdates 判断一次 extra 增量更新是否
// 显式启用了指纹收敛。仓储层据此在同一数据库事务内补齐随机种子。
func ShouldEnsureCodexFingerprintSeedForExtraUpdates(updates map[string]any) bool {
	if updates == nil {
		return false
	}
	raw, ok := updates[codexFingerprintModeExtraKey]
	if !ok {
		return false
	}
	switch codexFingerprintMode(strings.TrimSpace(fmt.Sprint(raw))) {
	case codexFingerprintDevice, codexFingerprintSession, codexFingerprintCockpit, codexFingerprintFull:
		return true
	default:
		return false
	}
}

// GetCodexFingerprintMode 从账号 extra JSON 读取指纹收敛模式。
// 未设置、空值或非法值保持关闭；身份改写只能由管理员显式开启。
func (a *Account) GetCodexFingerprintMode() codexFingerprintMode {
	if a == nil || !a.IsOpenAIOAuth() {
		return codexFingerprintOff
	}
	var raw string
	if a.Extra != nil {
		// Go 调用方可能写入命名类型 codexFingerprintMode，
		// 而从 JSON 加载的值仍为普通字符串。
		raw = strings.TrimSpace(fmt.Sprint(a.Extra[codexFingerprintModeExtraKey]))
	}
	switch codexFingerprintMode(raw) {
	case codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintCockpit, codexFingerprintFull:
		return codexFingerprintMode(raw)
	default:
		return codexFingerprintOff
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

var codexFallbackUUIDv7 sync.Map

const (
	codexIdentityBindingIdleTTL       = 7 * 24 * time.Hour
	codexIdentityBindingTouchEvery    = 5 * time.Minute
	codexIdentityHotCacheTTL          = 24 * time.Hour
	codexIdentityBindingMaxEntries    = 1024
	codexTurnLineageBindingIdleTTL    = 30 * 24 * time.Hour
	codexTurnLineageBindingMaxEntries = 8192
	// 等锁和存储共享预算；超时返回错误，不以跳过落库继续转发换取低延迟。
	codexIdentityPersistenceTimeout = 5 * time.Second
)

// CodexIdentityBindingsExtraKey stores complete UUIDv7 values keyed by a
// hashed conversation seed.  The binding is persisted with the OAuth account
// so a process restart or a second gateway instance reuses the same UUID
// instead of creating a new timestamped identity and breaking cache affinity.
const CodexIdentityBindingsExtraKey = "codex_identity_bindings_v1"

// CodexTurnLineageBindingsExtraKey 单独保存 Cockpit 回合图的 UUIDv7 映射，
// 避免高频 turn 淘汰 installation/session/thread 等低频账号身份绑定。
const CodexTurnLineageBindingsExtraKey = "codex_turn_lineage_bindings_v1"

// DiscardCodexFingerprintRuntimeBindings 清除只能由网关维护的 UUIDv7 映射。
// 通用创建、编辑和批量更新入口不得接受客户端提供的运行态绑定。
func DiscardCodexFingerprintRuntimeBindings(extra map[string]any) {
	delete(extra, CodexIdentityBindingsExtraKey)
	delete(extra, CodexTurnLineageBindingsExtraKey)
}

type codexIdentityBinding struct {
	UUID         string `json:"uuid"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	LastUsedAtMS int64  `json:"last_used_at_ms"`
}

var codexIdentityBindingLocks sync.Map    // account ID -> *codexIdentityMutex
var codexIdentityPersistedHashes sync.Map // account ID -> sha256 of bindings JSON
var codexIdentityHotCache sync.Map        // account+seed -> codexIdentityHotBinding
var codexIdentityHotCacheOps atomic.Uint64

// codexPromptCacheCarry 保存同一会话最近一次明确的客户端缓存键。
// 普通 Responses 允许同窗口续用、相邻压缩窗口单向继承；旧 compact 仍保持独立协议。
var codexPromptCacheCarry = struct {
	sync.Mutex
	items map[string]codexPromptCacheCarryEntry
}{items: make(map[string]codexPromptCacheCarryEntry)}

// codexTurnStartedBindings 为显式回合维护稳定的开始时间。
// 这是短期运行态，不能作为账号身份或缓存键的一部分。
var codexTurnStartedBindings = struct {
	sync.Mutex
	items map[string]codexTurnStartedBinding
}{items: make(map[string]codexTurnStartedBinding)}

type codexTurnStartedBinding struct {
	Value          int64
	LastUsedAt     int64
	ClientProvided bool
}

const codexTurnStartedBindingMaxEntries = 8192

type codexPromptCacheCarryEntry struct {
	Key        string
	WindowID   string
	InBody     bool
	LastUsedAt int64
}

const codexPromptCacheCarryMaxEntries = 1024

type codexIdentityHotBinding struct {
	UUID         string
	LastUsedAtMS int64
}

func codexPromptCacheCarryKey(accountID int64, sessionID, threadID, windowID string) string {
	return fmt.Sprintf("%d:%s:%s:%s", accountID, strings.TrimSpace(sessionID), strings.TrimSpace(threadID), strings.TrimSpace(windowID))
}

// 实例键与旧代数键分域；不依赖落库前可能被协调替换的出站 UUID。
func codexPromptCacheWindow(ids *codexFingerprintIDs) string {
	if ids.mode == codexFingerprintCockpit && ids.windowInstanceID != "" {
		return "instance:" + ids.windowInstanceID
	}
	return ids.windowID
}

func rememberCodexPromptCacheKey(account *Account, ids *codexFingerprintIDs, key string, inBody bool) {
	if account == nil || ids == nil || strings.TrimSpace(key) == "" {
		return
	}
	now := time.Now().UnixMilli()
	window := codexPromptCacheWindow(ids)
	cacheKey := codexPromptCacheCarryKey(account.ID, ids.sessionID, ids.threadID, window)
	codexPromptCacheCarry.Lock()
	defer codexPromptCacheCarry.Unlock()
	storeCodexPromptCacheKeyLocked(cacheKey, codexPromptCacheCarryEntry{Key: key, WindowID: window, InBody: inBody, LastUsedAt: now})
}

func codexTurnStartedBindingKey(account *Account, mode codexFingerprintMode, sessionID, originalTurnID, turnID string) string {
	if account == nil {
		return ""
	}
	turnKey := strings.TrimSpace(originalTurnID)
	if turnKey == "" {
		turnKey = strings.TrimSpace(turnID)
	}
	if turnKey == "" {
		return ""
	}
	return fmt.Sprintf("%d:%s:%s:%s", account.ID, mode, strings.TrimSpace(sessionID), turnKey)
}

// resolveCodexTurnStartedAt 保证同一账号、会话和逻辑 turn 只使用一个开始时间。
// 客户端第一次提供的有效时间优先；后续请求即使省略也沿用该时间。
func resolveCodexTurnStartedAt(account *Account, mode codexFingerprintMode, sessionID, originalTurnID, turnID string, provided int64, providedPresent bool) int64 {
	now := time.Now().UnixMilli()
	key := codexTurnStartedBindingKey(account, mode, sessionID, originalTurnID, turnID)
	if key == "" {
		if providedPresent {
			return provided
		}
		return now
	}

	codexTurnStartedBindings.Lock()
	defer codexTurnStartedBindings.Unlock()
	cutoff := now - codexIdentityBindingIdleTTL.Milliseconds()
	// 热命中只检查目标的 TTL；过期清理和容量整理留给新回合，避免每次扫全局表。
	if entry, ok := codexTurnStartedBindings.items[key]; ok && (entry.LastUsedAt == 0 || entry.LastUsedAt >= cutoff) {
		// 兜底时间不是客户端已确认时间；只允许首次有效客户端值替换一次。
		if providedPresent && !entry.ClientProvided {
			entry.Value, entry.ClientProvided = provided, true
		}
		entry.LastUsedAt = now
		codexTurnStartedBindings.items[key] = entry
		return entry.Value
	}
	for candidate, entry := range codexTurnStartedBindings.items {
		if entry.LastUsedAt > 0 && entry.LastUsedAt < cutoff {
			delete(codexTurnStartedBindings.items, candidate)
		}
	}
	value := now
	if providedPresent {
		value = provided
	}
	if len(codexTurnStartedBindings.items) >= codexTurnStartedBindingMaxEntries {
		oldestKey := ""
		oldest := int64(math.MaxInt64)
		for candidate, entry := range codexTurnStartedBindings.items {
			if entry.LastUsedAt < oldest {
				oldestKey, oldest = candidate, entry.LastUsedAt
			}
		}
		if oldestKey != "" {
			delete(codexTurnStartedBindings.items, oldestKey)
		}
	}
	codexTurnStartedBindings.items[key] = codexTurnStartedBinding{
		Value: value, LastUsedAt: now, ClientProvided: providedPresent,
	}
	return value
}

// 显式绑定和跨窗口继承共用容量控制，调用方须持有缓存锁。
func storeCodexPromptCacheKeyLocked(cacheKey string, entry codexPromptCacheCarryEntry) {
	if _, exists := codexPromptCacheCarry.items[cacheKey]; !exists && len(codexPromptCacheCarry.items) >= codexPromptCacheCarryMaxEntries {
		var oldestKey string
		oldest := int64(math.MaxInt64)
		for candidate, existing := range codexPromptCacheCarry.items {
			if existing.LastUsedAt <= oldest {
				oldestKey, oldest = candidate, existing.LastUsedAt
			}
		}
		if oldestKey != "" {
			delete(codexPromptCacheCarry.items, oldestKey)
		}
	}
	codexPromptCacheCarry.items[cacheKey] = entry
}

func loadCodexPromptCacheKey(account *Account, ids *codexFingerprintIDs) (codexPromptCacheCarryEntry, bool) {
	if account == nil || ids == nil {
		return codexPromptCacheCarryEntry{}, false
	}
	window := codexPromptCacheWindow(ids)
	cacheKey := codexPromptCacheCarryKey(account.ID, ids.sessionID, ids.threadID, window)
	now := time.Now().UnixMilli()
	codexPromptCacheCarry.Lock()
	defer codexPromptCacheCarry.Unlock()
	load := func(key, windowID string) (codexPromptCacheCarryEntry, bool) {
		entry, ok := codexPromptCacheCarry.items[key]
		if !ok {
			return codexPromptCacheCarryEntry{}, false
		}
		if now-entry.LastUsedAt > codexIdentityBindingIdleTTL.Milliseconds() || entry.WindowID != windowID {
			delete(codexPromptCacheCarry.items, key)
			return codexPromptCacheCarryEntry{}, false
		}
		return entry, true
	}
	entry, ok := load(cacheKey, window)
	if !ok && ids.extendedTurnIdentity && ids.windowNumber > 0 {
		previousWindow := fmt.Sprintf("%s:%d", ids.threadID, ids.windowNumber-1)
		if ids.windowInstanceID != "" {
			// 实例路径只认明确前驱；代数相邻不证明属于同一压缩分支。
			previousWindow = ""
			if ids.originalPreviousWindowID != "" && ids.originalPreviousWindowID != ids.windowInstanceID {
				previousWindow = "instance:" + ids.originalPreviousWindowID
			}
		}
		if previousWindow != "" {
			entry, ok = load(codexPromptCacheCarryKey(account.ID, ids.sessionID, ids.threadID, previousWindow), previousWindow)
		}
	}
	if !ok {
		return codexPromptCacheCarryEntry{}, false
	}
	entry.WindowID = window
	entry.LastUsedAt = now
	storeCodexPromptCacheKeyLocked(cacheKey, entry)
	return entry, true
}

// codexIdentityMutex 保持生成与持久化共用的账号互斥域，持久化等待支持取消。
type codexIdentityMutex struct {
	gate *semaphore.Weighted
}

func (m *codexIdentityMutex) Lock()   { _ = m.gate.Acquire(context.Background(), 1) }
func (m *codexIdentityMutex) Unlock() { m.gate.Release(1) }
func (m *codexIdentityMutex) LockContext(ctx context.Context) error {
	return m.gate.Acquire(ctx, 1)
}

func codexIdentityBindingLock(accountID int64) *codexIdentityMutex {
	if existing, ok := codexIdentityBindingLocks.Load(accountID); ok {
		if mutex, ok := existing.(*codexIdentityMutex); ok {
			return mutex
		}
	}
	created := &codexIdentityMutex{gate: semaphore.NewWeighted(1)}
	actual, _ := codexIdentityBindingLocks.LoadOrStore(accountID, created)
	if mutex, ok := actual.(*codexIdentityMutex); ok {
		return mutex
	}
	return created
}

func codexIdentitySeedKey(seed string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(seed)))
	return fmt.Sprintf("%x", h[:])
}

func codexIdentityOwnerID(account *Account) int64 {
	if account != nil && account.ParentAccountID != nil && *account.ParentAccountID != 0 {
		return *account.ParentAccountID
	}
	if account == nil {
		return 0
	}
	return account.ID
}

func codexIdentityHotKey(account *Account, seed string) string {
	return fmt.Sprintf("%d:%s", codexIdentityOwnerID(account), codexIdentitySeedKey(seed))
}

func deleteCodexIdentityHotCacheSeed(seedKey string) {
	codexIdentityHotCache.Range(func(key, _ any) bool {
		keyString, ok := key.(string)
		if ok && (keyString == seedKey || strings.HasSuffix(keyString, ":"+seedKey)) {
			codexIdentityHotCache.Delete(key)
		}
		return true
	})
}

func readCodexUUIDv7Bindings(account *Account, extraKey string) map[string]any {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[extraKey]
	if !ok {
		return nil
	}
	if bindings, ok := raw.(map[string]any); ok {
		return bindings
	}
	return nil
}

func readCodexIdentityBindings(account *Account) map[string]any {
	return readCodexUUIDv7Bindings(account, CodexIdentityBindingsExtraKey)
}

func readCodexTurnLineageBindings(account *Account) map[string]any {
	return readCodexUUIDv7Bindings(account, CodexTurnLineageBindingsExtraKey)
}

// ensureCodexUUIDv7Bindings 返回可写绑定集合，并在首次写入时挂到账号 Extra。
func ensureCodexUUIDv7Bindings(account *Account, extraKey string) map[string]any {
	bindings := readCodexUUIDv7Bindings(account, extraKey)
	if bindings != nil {
		return bindings
	}
	bindings = make(map[string]any)
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	account.Extra[extraKey] = bindings
	return bindings
}

func parseCodexIdentityBinding(raw any) (codexIdentityBinding, bool) {
	switch value := raw.(type) {
	case string:
		if parsed, err := uuid.Parse(value); err == nil && parsed.Version() == uuid.Version(7) && parsed.Variant() == uuid.RFC4122 {
			return codexIdentityBinding{UUID: canonicalCodexBindingUUID(value, parsed)}, true
		}
	case map[string]any:
		candidate, _ := value["uuid"].(string)
		parsed, err := uuid.Parse(strings.TrimSpace(candidate))
		if err != nil || parsed.Version() != uuid.Version(7) || parsed.Variant() != uuid.RFC4122 {
			return codexIdentityBinding{}, false
		}
		created, _ := value["created_at_ms"].(float64)
		lastUsed, _ := value["last_used_at_ms"].(float64)
		return codexIdentityBinding{UUID: canonicalCodexBindingUUID(candidate, parsed), CreatedAtMS: int64(created), LastUsedAtMS: int64(lastUsed)}, true
	case codexIdentityBinding:
		parsed, err := uuid.Parse(value.UUID)
		if err == nil && parsed.Version() == uuid.Version(7) && parsed.Variant() == uuid.RFC4122 {
			return value, true
		}
	}
	return codexIdentityBinding{}, false
}

// 数据库通常已保存规范 UUID；校验后复用字符串，只有历史非规范写法才重新格式化。
func canonicalCodexBindingUUID(value string, parsed uuid.UUID) string {
	if len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' {
		canonical := true
		for i := range value {
			if value[i] >= 'A' && value[i] <= 'F' {
				canonical = false
				break
			}
		}
		if canonical {
			return value
		}
	}
	return parsed.String()
}

// deriveStableUUIDv7ForAccount first consults the account's durable binding.
// On a miss it generates exactly once using the current Unix millisecond and
// records the complete UUID in Extra.  Callers persist the changed Extra via
// AccountRepository after the request snapshot is built.
func deriveStableUUIDv7ForAccount(account *Account, seed string) string {
	return deriveStableUUIDv7ForAccountStore(
		account,
		seed,
		CodexIdentityBindingsExtraKey,
		codexIdentityBindingIdleTTL,
		codexIdentityBindingMaxEntries,
	)
}

// loadCodexUUIDv7Binding 从指定账号级存储或热缓存读取 UUIDv7 绑定。
// 调用方持有账号锁；命中时继续沿用既有触摸、裁剪和回填规则。
func loadCodexUUIDv7Binding(account *Account, seed, extraKey string, idleTTL time.Duration, maxEntries int, nowMS int64) (string, bool) {
	key := codexIdentitySeedKey(seed)
	bindings := readCodexUUIDv7Bindings(account, extraKey)
	if bindings != nil {
		// 活跃目标且集合未超限时无需完整裁剪；缺省/过期目标和提交仍执行原有清理。
		binding, valid := parseCodexIdentityBinding(bindings[key])
		lastUsed := codexIdentityBindingRecency(binding)
		if !valid || (lastUsed > 0 && nowMS-lastUsed >= idleTTL.Milliseconds()) || len(bindings) > maxEntries {
			if pruneCodexUUIDv7Bindings(bindings, nowMS, key, idleTTL, maxEntries) {
				account.codexIdentityBindingsDirty = true
			}
		}
		if binding, ok := parseCodexIdentityBinding(bindings[key]); ok {
			changed := false
			if binding.CreatedAtMS == 0 {
				binding.CreatedAtMS = nowMS
				changed = true
			}
			if binding.LastUsedAtMS == 0 || nowMS-binding.LastUsedAtMS >= codexIdentityBindingTouchEvery.Milliseconds() {
				binding.LastUsedAtMS = nowMS
				changed = true
			}
			if changed {
				bindings[key] = binding
				account.codexIdentityBindingsDirty = true
			}
			codexIdentityHotCache.Store(codexIdentityHotKey(account, seed), codexIdentityHotBinding{UUID: binding.UUID, LastUsedAtMS: nowMS})
			sweepCodexIdentityHotCache()
			return binding.UUID, true
		}
	}
	hotKey := codexIdentityHotKey(account, seed)
	if raw, ok := codexIdentityHotCache.Load(hotKey); ok {
		if hot, valid := raw.(codexIdentityHotBinding); valid && hot.UUID != "" && (hot.LastUsedAtMS == 0 || nowMS-hot.LastUsedAtMS < codexIdentityHotCacheTTL.Milliseconds()) {
			bindings = ensureCodexUUIDv7Bindings(account, extraKey)
			bindings[key] = codexIdentityBinding{UUID: hot.UUID, CreatedAtMS: nowMS, LastUsedAtMS: nowMS}
			account.codexIdentityBindingsDirty = true
			codexIdentityHotCache.Store(hotKey, codexIdentityHotBinding{UUID: hot.UUID, LastUsedAtMS: nowMS})
			return hot.UUID, true
		}
		codexIdentityHotCache.Delete(hotKey)
	}
	return "", false
}

// storeCodexUUIDv7Binding 写入已确定的完整 UUID，并复用既有裁剪和热缓存规则。
func storeCodexUUIDv7Binding(account *Account, seed, extraKey, value string, idleTTL time.Duration, maxEntries int, nowMS int64) {
	key := codexIdentitySeedKey(seed)
	bindings := ensureCodexUUIDv7Bindings(account, extraKey)
	bindings[key] = codexIdentityBinding{UUID: value, CreatedAtMS: nowMS, LastUsedAtMS: nowMS}
	account.codexIdentityBindingsDirty = true
	codexIdentityHotCache.Store(codexIdentityHotKey(account, seed), codexIdentityHotBinding{UUID: value, LastUsedAtMS: nowMS})
	pruneCodexUUIDv7Bindings(bindings, nowMS, key, idleTTL, maxEntries)
	sweepCodexIdentityHotCache()
}

// storeCodexUUIDv7Sibling 为历史单边根保留已有键，再独立建立缺失侧绑定。
func storeCodexUUIDv7Sibling(account *Account, existingSeed, newSeed, extraKey, value string, idleTTL time.Duration, maxEntries int, nowMS int64) {
	bindings := ensureCodexUUIDv7Bindings(account, extraKey)
	if maxEntries >= 1 {
		pruneCodexUUIDv7Bindings(bindings, nowMS, codexIdentitySeedKey(existingSeed), idleTTL, maxEntries-1)
	}
	storeCodexUUIDv7Binding(account, newSeed, extraKey, value, idleTTL, maxEntries, nowMS)
}

// storeCodexUUIDv7Pair 为新根预留两个槽位后一次生成、双键写入，避免满容量时拆散共享关系。
func storeCodexUUIDv7Pair(account *Account, firstSeed, secondSeed, extraKey, value string, idleTTL time.Duration, maxEntries int, nowMS int64) {
	bindings := ensureCodexUUIDv7Bindings(account, extraKey)
	if maxEntries >= 2 {
		pruneCodexUUIDv7Bindings(bindings, nowMS, "", idleTTL, maxEntries-2)
	}
	binding := codexIdentityBinding{UUID: value, CreatedAtMS: nowMS, LastUsedAtMS: nowMS}
	bindings[codexIdentitySeedKey(firstSeed)] = binding
	bindings[codexIdentitySeedKey(secondSeed)] = binding
	account.codexIdentityBindingsDirty = true
	codexIdentityHotCache.Store(codexIdentityHotKey(account, firstSeed), codexIdentityHotBinding{UUID: value, LastUsedAtMS: nowMS})
	codexIdentityHotCache.Store(codexIdentityHotKey(account, secondSeed), codexIdentityHotBinding{UUID: value, LastUsedAtMS: nowMS})
	sweepCodexIdentityHotCache()
}

// deriveStableUUIDv7ForAccountStore 在指定账号级存储中维护 UUIDv7 绑定。
func deriveStableUUIDv7ForAccountStore(account *Account, seed, extraKey string, idleTTL time.Duration, maxEntries int) string {
	seed = strings.TrimSpace(seed)
	if account == nil || seed == "" {
		return deriveStableUUIDv7(seed)
	}
	if !account.codexIdentityLockHeld {
		lock := codexIdentityBindingLock(account.ID)
		lock.Lock()
		defer lock.Unlock()
	}
	nowMS := time.Now().UnixMilli()
	if value, ok := loadCodexUUIDv7Binding(account, seed, extraKey, idleTTL, maxEntries, nowMS); ok {
		return value
	}
	value := newCodexUUIDv7().String()
	storeCodexUUIDv7Binding(account, seed, extraKey, value, idleTTL, maxEntries, nowMS)
	return value
}

// pruneCodexUUIDv7Bindings 对指定绑定集合执行滑动过期和 LRU 容量控制。
func pruneCodexUUIDv7Bindings(bindings map[string]any, nowMS int64, protectedKey string, idleTTL time.Duration, maxEntries int, snapshots ...*codexFingerprintSnapshotBindings) bool {
	if len(bindings) == 0 {
		return false
	}
	changed := false
	for key, raw := range bindings {
		binding, ok := parseCodexIdentityBinding(raw)
		if !ok {
			delete(bindings, key)
			changed = true
			continue
		}
		// 必须在过期或容量淘汰前记住引用，之后的提交才能识别快照依赖消失。
		if len(snapshots) > 0 && snapshots[0] != nil {
			snapshot := snapshots[0]
			if _, used := snapshot.values[binding.UUID]; used {
				if snapshot.bindings == nil {
					snapshot.bindings = make(map[string]string)
				}
				snapshot.bindings[key] = binding.UUID
			}
		}
		lastUsed := binding.LastUsedAtMS
		if lastUsed == 0 {
			lastUsed = binding.CreatedAtMS
		}
		if lastUsed > 0 && nowMS-lastUsed >= idleTTL.Milliseconds() {
			delete(bindings, key)
			deleteCodexIdentityHotCacheSeed(key)
			changed = true
			continue
		}
		if binding.LastUsedAtMS == 0 {
			binding.LastUsedAtMS = nowMS
			bindings[key] = binding
			changed = true
		}
		// 同次提交后续合并直接使用类型化值，JSON 载体保持相同字段与数值。
		if _, typed := raw.(codexIdentityBinding); !typed {
			bindings[key] = binding
		}
	}
	for len(bindings) > maxEntries {
		oldestKey := ""
		var oldest int64
		for key, raw := range bindings {
			if key == protectedKey && len(bindings) > 1 {
				continue
			}
			binding, ok := parseCodexIdentityBinding(raw)
			if !ok {
				oldestKey = key
				break
			}
			used := binding.LastUsedAtMS
			if used == 0 {
				used = binding.CreatedAtMS
			}
			if oldestKey == "" || used < oldest || (used == oldest && key < oldestKey) {
				oldestKey, oldest = key, used
			}
		}
		if oldestKey == "" {
			break
		}
		delete(bindings, oldestKey)
		deleteCodexIdentityHotCacheSeed(oldestKey)
		changed = true
	}
	return changed
}

func sweepCodexIdentityHotCache() {
	if codexIdentityHotCacheOps.Add(1)%256 != 0 {
		return
	}
	cutoff := time.Now().Add(-codexIdentityHotCacheTTL).UnixMilli()
	codexIdentityHotCache.Range(func(key, raw any) bool {
		binding, ok := raw.(codexIdentityHotBinding)
		if !ok || binding.LastUsedAtMS == 0 || binding.LastUsedAtMS < cutoff {
			codexIdentityHotCache.Delete(key)
		}
		return true
	})
}

func codexIdentityBindingRecency(binding codexIdentityBinding) int64 {
	if binding.LastUsedAtMS != 0 {
		return binding.LastUsedAtMS
	}
	return binding.CreatedAtMS
}

// mergeCodexIdentityBindings keeps the durable value with the newest observed
// activity for each seed.  Requests may hold distinct Account snapshots, so a
// blind current-wins merge can resurrect an older UUID or erase a newer touch.
func mergeCodexIdentityBindings(latest, current map[string]any) map[string]any {
	merged := make(map[string]any, len(latest)+len(current))
	for key, value := range latest {
		merged[key] = value
	}
	for key, currentRaw := range current {
		latestRaw, exists := merged[key]
		if !exists {
			merged[key] = currentRaw
			continue
		}
		currentBinding, currentOK := parseCodexIdentityBinding(currentRaw)
		latestBinding, latestOK := parseCodexIdentityBinding(latestRaw)
		switch {
		case currentOK && !latestOK:
			merged[key] = currentRaw
		case !currentOK && latestOK:
			// Keep the valid durable value; prune will remove malformed entries.
		case currentOK && latestOK && codexIdentityBindingRecency(currentBinding) > codexIdentityBindingRecency(latestBinding):
			merged[key] = currentRaw
		}
	}
	return merged
}

type codexUUIDv7BindingStore struct {
	extraKey   string
	idleTTL    time.Duration
	maxEntries int
}

var codexUUIDv7BindingStores = []codexUUIDv7BindingStore{
	{extraKey: CodexIdentityBindingsExtraKey, idleTTL: codexIdentityBindingIdleTTL, maxEntries: codexIdentityBindingMaxEntries},
	{extraKey: CodexTurnLineageBindingsExtraKey, idleTTL: codexTurnLineageBindingIdleTTL, maxEntries: codexTurnLineageBindingMaxEntries},
}

type codexIdentityPersistenceState struct {
	updates        map[string]any
	hotUpdates     []codexIdentityHotUpdate
	reconciled     []codexFingerprintIDs
	matchesDurable bool
}

// restoreCodexIdentityBindingsBeforeResolve 用数据库行锁内读到的绑定覆盖请求旧快照。
// 只替换运行态绑定集合，账号模式、TLS 配置及其它 Extra 仍使用本次调度快照。
func restoreCodexIdentityBindingsBeforeResolve(account, latest *Account) {
	if account == nil || latest == nil {
		return
	}
	extra := make(map[string]any, len(account.Extra)+len(codexUUIDv7BindingStores))
	for key, value := range account.Extra {
		extra[key] = value
	}
	for _, store := range codexUUIDv7BindingStores {
		bindings := readCodexUUIDv7Bindings(latest, store.extraKey)
		if bindings == nil {
			delete(extra, store.extraKey)
			continue
		}
		cloned := make(map[string]any, len(bindings))
		for key, value := range bindings {
			cloned[key] = value
		}
		extra[store.extraKey] = cloned
	}
	account.Extra = extra
	account.codexIdentityBindingsDirty = false
}

// buildCodexIdentityPersistenceState 合并、裁剪并准备提交结果，但不发布热缓存或修改出站快照。
// 调用方只有在数据库写入或行锁事务成功后才能调用 commitCodexIdentityPersistenceState。
func buildCodexIdentityPersistenceState(account, latest *Account, snapshots ...*codexFingerprintIDs) (codexIdentityPersistenceState, error) {
	state := codexIdentityPersistenceState{
		updates:        make(map[string]any, len(codexUUIDv7BindingStores)),
		matchesDurable: true,
	}
	if account == nil || account.Extra == nil {
		return state, nil
	}
	nowMS := time.Now().UnixMilli()
	snapshotValues := codexFingerprintSnapshotValues(snapshots)
	var snapshotSelections map[string]string
	hotPrefix := strconv.FormatInt(codexIdentityOwnerID(account), 10) + ":"
	for _, store := range codexUUIDv7BindingStores {
		if _, exists := account.Extra[store.extraKey]; !exists {
			continue
		}
		bindings := readCodexUUIDv7Bindings(account, store.extraKey)
		if bindings == nil {
			bindings = make(map[string]any)
		}
		snapshotBindings := codexFingerprintSnapshotBindings{values: snapshotValues}
		if pruneCodexUUIDv7Bindings(bindings, nowMS, "", store.idleTTL, store.maxEntries, &snapshotBindings) {
			state.matchesDurable = false
		}
		latestBindings := readCodexUUIDv7Bindings(latest, store.extraKey)
		if latestBindings != nil {
			if pruneCodexUUIDv7Bindings(latestBindings, nowMS, "", store.idleTTL, store.maxEntries) {
				state.matchesDurable = false
			}
			bindings = mergeCodexIdentityBindings(latestBindings, bindings)
			pruneCodexUUIDv7Bindings(bindings, nowMS, "", store.idleTTL, store.maxEntries)
		}
		if latestBindings == nil || len(latestBindings) != len(bindings) {
			state.matchesDurable = false
		}
		// 比较最新数据库值时同时收集缓存变更；成功后不再完整遍历一次集合。
		for key, raw := range bindings {
			binding, _ := parseCodexIdentityBinding(raw)
			durable, valid := parseCodexIdentityBinding(latestBindings[key])
			if !valid || durable != binding {
				state.matchesDurable = false
			}
			state.hotUpdates = collectCodexIdentityHotUpdate(state.hotUpdates, hotPrefix+key, binding)
		}
		snapshotSelections = collectCodexFingerprintSnapshotChanges(snapshotBindings.bindings, bindings, snapshotSelections)
		account.Extra[store.extraKey] = bindings
		state.updates[store.extraKey] = bindings
	}
	reconciled, err := reconcileCodexFingerprintSnapshots(snapshots, snapshotSelections)
	if err != nil {
		return codexIdentityPersistenceState{}, err
	}
	state.reconciled = reconciled
	return state, nil
}

// commitCodexIdentityPersistenceState 只在持久化事务成功后发布最终状态。
func commitCodexIdentityPersistenceState(account *Account, snapshots []*codexFingerprintIDs, state codexIdentityPersistenceState) {
	publishCodexIdentityHotBindings(state.hotUpdates)
	commitCodexFingerprintSnapshots(snapshots, state.reconciled)
	if account != nil {
		account.codexIdentityBindingsDirty = false
	}
}

// persistCodexIdentityBindings 一次性持久化账号身份和 Cockpit 回合图绑定。
// @project-doc docs/interfaces/openai_upstream.md#codex_identity_persistence
// 与最新持久化值逐项比较让无变化的热路径保持只读，账号锁和写前合并协调并发快照。
func persistCodexIdentityBindings(ctx context.Context, repo AccountRepository, account *Account, snapshots ...*codexFingerprintIDs) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// Lightweight unit-test repositories and optional deployments may not
			// implement Extra persistence.  Keep the already-built request snapshot
			// usable while surfacing the condition to callers that choose to log it.
			err = fmt.Errorf("persist codex identity bindings: repository unavailable: %v", recovered)
		}
	}()
	if repo == nil || account == nil || account.ID == 0 {
		return nil
	}
	if !account.codexIdentityLockHeld {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, codexIdentityPersistenceTimeout)
		defer cancel()
		lock := codexIdentityBindingLock(account.ID)
		finishWait := latencytrace.Start(ctx, "identity_lock_wait")
		err = lock.LockContext(ctx)
		finishWait(err)
		if err != nil {
			return fmt.Errorf("wait for codex identity persistence: %w", err)
		}
		defer lock.Unlock()
	}
	if account.Extra == nil {
		return nil
	}
	hasBindings := false
	for _, store := range codexUUIDv7BindingStores {
		if _, exists := account.Extra[store.extraKey]; exists {
			hasBindings = true
			break
		}
	}
	if !hasBindings {
		return nil
	}

	// 生产仓储只读取绑定所需的账号 ID/Extra；旧适配器仍保持原有读写契约。
	finishRead := latencytrace.Start(ctx, "identity_binding_read")
	var latest *Account
	if reader, ok := repo.(CodexIdentityBindingsReader); ok {
		latest, err = reader.GetCodexIdentityBindings(ctx, account.ID)
	} else {
		latest, err = repo.GetByID(ctx, account.ID)
	}
	finishRead(err)
	if err != nil {
		return fmt.Errorf("load latest account for codex identity bindings: %w", err)
	}
	finishMerge := latencytrace.Start(ctx, "identity_binding_merge")
	state, err := buildCodexIdentityPersistenceState(account, latest, snapshots...)
	if err != nil {
		finishMerge(err)
		return err
	}
	if err := ctx.Err(); err != nil {
		finishMerge(err)
		return err
	}
	hashKey := strconv.FormatInt(account.ID, 10)
	// 保留首次成功登记；后续已与最新数据库完全一致时，不再序列化整份集合算哈希。
	// 判定仍以本次数据库读取为准，不以历史哈希掩盖其他实例写入。
	if _, committed := codexIdentityPersistedHashes.Load(hashKey); committed && state.matchesDurable {
		finishMerge(nil)
		commitCodexIdentityPersistenceState(account, snapshots, state)
		return nil
	}
	// 过期或损坏的集合仍以空对象落库，确保旧值实际被清除。
	encoded, err := json.Marshal(state.updates)
	finishMerge(err)
	if err != nil {
		return fmt.Errorf("marshal codex UUIDv7 bindings: %w", err)
	}
	hash := sha256.Sum256(encoded)
	hashString := fmt.Sprintf("%x", hash[:])
	finishWrite := latencytrace.Start(ctx, "identity_binding_write")
	err = repo.UpdateExtra(ctx, account.ID, state.updates)
	finishWrite(err)
	if err != nil {
		return fmt.Errorf("persist codex identity bindings: %w", err)
	}
	codexIdentityPersistedHashes.Store(hashKey, hashString)
	commitCodexIdentityPersistenceState(account, snapshots, state)
	return nil
}

// codexUUIDv7Context 使用 41 位随机初值和 42 位递增计数器。
// 新毫秒重新播种；时钟停顿或回退时沿用逻辑毫秒并递增。
const codexUUIDv7MaxCounter = (uint64(1) << 42) - 1

type codexUUIDv7Context struct {
	mu          sync.Mutex
	initialized bool
	timestampMS uint64
	lastSeedMS  uint64
	counter     uint64
}

var codexUUIDv7Shared codexUUIDv7Context

func codexUUIDv7Random41() uint64 {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// This path is only reached when the OS random source fails. Keep the
		// shape valid and let the UUID random tail provide additional entropy.
		return uint64(time.Now().UnixNano()) & ((uint64(1) << 41) - 1)
	}
	return binary.BigEndian.Uint64(b[:]) & ((uint64(1) << 41) - 1)
}

// encodeCodexUUIDv7 将 42 位计数器无损放入 RFC 9562 的可用位。
// 高 12 位位于 rand_a，低 30 位紧跟 variant，最后 32 位独立随机。
// 显式避开 version/variant，避免掩码覆盖计数位后在进位处破坏排序。
func encodeCodexUUIDv7(timestampMS, counter uint64, randomBytes [16]byte) uuid.UUID {
	counter &= codexUUIDv7MaxCounter
	var out [16]byte
	out[0] = byte(timestampMS >> 40)
	out[1] = byte(timestampMS >> 32)
	out[2] = byte(timestampMS >> 24)
	out[3] = byte(timestampMS >> 16)
	out[4] = byte(timestampMS >> 8)
	out[5] = byte(timestampMS)
	out[6] = 0x70 | byte(counter>>38)
	out[7] = byte(counter >> 30)
	out[8] = 0x80 | byte(counter>>24)&0x3f
	out[9] = byte(counter >> 16)
	out[10] = byte(counter >> 8)
	out[11] = byte(counter)
	copy(out[12:], randomBytes[6:10])
	return uuid.UUID(out)
}

// next 在同一个锁内分配逻辑时间和计数器；独立上下文可确定性验证时钟边界。
func (c *codexUUIDv7Context) next(nowMS uint64) (uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.initialized || nowMS > c.lastSeedMS {
		c.initialized = true
		c.timestampMS = nowMS
		c.lastSeedMS = nowMS
		c.counter = codexUUIDv7Random41()
	} else {
		c.counter++
		if c.counter > codexUUIDv7MaxCounter {
			c.timestampMS++
			c.lastSeedMS = c.timestampMS
			c.counter = codexUUIDv7Random41()
		}
	}
	return c.timestampMS, c.counter
}

func newCodexUUIDv7() uuid.UUID {
	now := time.Now().UnixMilli()
	if now < 0 {
		now = 0
	}
	timestampMS, counter := codexUUIDv7Shared.next(uint64(now))

	var randomBytes [16]byte
	if _, err := cryptorand.Read(randomBytes[:]); err != nil {
		return uuid.Must(uuid.NewV7())
	}
	return encodeCodexUUIDv7(timestampMS, counter, randomBytes)
}

// deriveStableUUIDv7 为缺少官方身份字段的桥接请求生成一次 UUIDv7。
// 生成器采用无损 42 位计数器和独立 32 位随机尾部；
// 结果按种子缓存，使同一进程内的同一账号/对话保持稳定，避免每轮请求改变缓存亲和。
func deriveStableUUIDv7(seed string) string {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return ""
	}
	if existing, ok := codexFallbackUUIDv7.Load(seed); ok {
		if value, valid := existing.(string); valid {
			return value
		}
		codexFallbackUUIDv7.Delete(seed)
	}
	generated := newCodexUUIDv7().String()
	actual, _ := codexFallbackUUIDv7.LoadOrStore(seed, generated)
	if value, valid := actual.(string); valid {
		return value
	}
	codexFallbackUUIDv7.Store(seed, generated)
	return generated
}

// normalizeCodexWindowID 保留官方 <thread_id>:<generation> 线格式。
// 客户端缺少或携带旧式裸 UUID 时，按当前出站 thread_id 回退到首个窗口。
func normalizeCodexWindowID(raw, threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return ""
	}
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(raw, ":"); idx > 0 && idx < len(raw)-1 {
		generation := strings.TrimSpace(raw[idx+1:])
		if n, err := strconv.ParseUint(generation, 10, 64); err == nil {
			return threadID + ":" + strconv.FormatUint(n, 10)
		}
	}
	return threadID + ":0"
}

// 仅窗口标记缺省识别会话时去掉代数，避免压缩把同一个线程当成新会话。
func codexSessionSeedFromWindowID(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(raw, ":"); idx > 0 {
		if _, err := strconv.ParseUint(strings.TrimSpace(raw[idx+1:]), 10, 64); err == nil {
			return raw[:idx]
		}
	}
	return raw
}

func codexWindowGeneration(windowID string) uint64 {
	windowID = strings.TrimSpace(windowID)
	if idx := strings.LastIndex(windowID, ":"); idx >= 0 {
		if value := strings.TrimSpace(windowID[idx+1:]); value != "" {
			if n, err := strconv.ParseUint(value, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// resolveCodexWindowLineage 按窗口代数派生账号隔离的历史锚点。
// 保留 thread_id:generation 线格式；首窗口和前一窗口使用稳定 UUIDv7。
func resolveCodexWindowLineage(account *Account, threadID, windowID string) (uint64, string, string) {
	generation := codexWindowGeneration(windowID)
	first := resolveConvergedContextWindowID(account, threadID, strings.TrimSpace(threadID)+":0")
	previous := ""
	if generation > 0 {
		previous = resolveConvergedContextWindowID(account, threadID, fmt.Sprintf("%s:%d", strings.TrimSpace(threadID), generation-1))
	}
	return generation, first, previous
}

// 缺少客户端窗口实例的兼容路径保留旧代数绑定，不把它冒充为已知实例。
func resolveConvergedContextWindowID(account *Account, threadID, windowID string) string {
	if account == nil || strings.TrimSpace(threadID) == "" {
		return ""
	}
	generation := strconv.FormatUint(codexWindowGeneration(windowID), 10)
	seed := fmt.Sprintf("codex-context-window:%s:%s", strings.TrimSpace(threadID), generation)
	return deriveStableUUIDv7ForAccount(account, seed)
}

// 窗口及 first/previous 引用共用实例命名空间；代数回退后同号新实例不会合并。
// 旧绑定未记录原始 UUID，故不猜测迁移；新实例首次建立独立持久绑定。
func resolveCodexWindowInstance(account *Account, threadID, originalID string) string {
	if account == nil || strings.TrimSpace(threadID) == "" || strings.TrimSpace(originalID) == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account,
		fmt.Sprintf("codex-context-instance:v2:%s:%s", strings.TrimSpace(threadID), strings.TrimSpace(originalID)))
}

// EnsureCodexFingerprintSeed 为新建的 OpenAI OAuth 账号补齐随机种子。
// 已有种子保持不变，保证数据库备份、恢复和进程重启后身份继续稳定。
func EnsureCodexFingerprintSeed(account *Account) string {
	if account == nil || !account.IsOpenAIOAuth() {
		return ""
	}
	if seed, ok := normalizeCodexFingerprintSeed(account.GetExtraString(CodexFingerprintSeedExtraKey)); ok {
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[CodexFingerprintSeedExtraKey] = seed
		return seed
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	seed := uuid.NewString()
	account.Extra[CodexFingerprintSeedExtraKey] = seed
	return seed
}

// PrepareCodexFingerprintSeedForCreate 只为显式启用收敛的新账号准备种子。
// 根账号始终轮换外来种子，防止导入或复制复用另一账号身份；影子账号优先
// 保留父账号种子。关闭收敛的账号不持久化无用种子。
func PrepareCodexFingerprintSeedForCreate(account *Account) string {
	if account == nil || !account.IsOpenAIOAuth() {
		return ""
	}
	// 新记录只继承允许的父账号种子，不接受外部携带的身份或回合映射。
	DiscardCodexFingerprintRuntimeBindings(account.Extra)
	if account.ParentAccountID != nil {
		if seed, ok := normalizeCodexFingerprintSeed(account.GetExtraString(CodexFingerprintSeedExtraKey)); ok {
			account.Extra[CodexFingerprintSeedExtraKey] = seed
			return seed
		}
	}
	if !ShouldEnsureCodexFingerprintSeedForExtraUpdates(account.Extra) {
		delete(account.Extra, CodexFingerprintSeedExtraKey)
		return ""
	}
	if account.ParentAccountID != nil {
		return EnsureCodexFingerprintSeed(account)
	}
	if account.Extra == nil {
		account.Extra = make(map[string]any)
	}
	seed := uuid.NewString()
	account.Extra[CodexFingerprintSeedExtraKey] = seed
	return seed
}

// resolveCodexFingerprintSeed 只读取已持久化的账号随机种子。
// 运行时不再回退到本地 account.ID，避免跨部署确定性碰撞。
func resolveCodexFingerprintSeed(account *Account) string {
	if account == nil || !account.IsOpenAIOAuth() {
		return ""
	}
	seed, ok := normalizeCodexFingerprintSeed(account.GetExtraString(CodexFingerprintSeedExtraKey))
	if !ok {
		return ""
	}
	return seed
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
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv4("sub2api:codex-install-id:v2:" + seed)
}

// resolveConvergedSessionID 返回账号级恒定的 session_id。
// session/full 模式保留这一拓扑；Cockpit 使用对话级派生，避免多个独立
// 对话在同一账号下暴露为同一个长寿命 session。
func resolveConvergedSessionID(account *Account) string {
	if account == nil {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, "sub2api:codex-session-id:v3:"+seed)
}

// resolveConvergedCockpitSessionID derives one stable server-side session per
// Cockpit conversation while keeping the installation identity account-scoped.
func resolveConvergedCockpitSessionID(account *Account, conversationSeed string) string {
	bindingSeed := codexCockpitSessionBindingSeed(account, conversationSeed)
	if bindingSeed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, bindingSeed)
}

func codexCockpitSessionBindingSeed(account *Account, conversationSeed string) string {
	conversationSeed = strings.TrimSpace(conversationSeed)
	if account == nil || conversationSeed == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return fmt.Sprintf("sub2api:codex-cockpit-session-id:v2:%s:%s", seed, conversationSeed)
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(account *Account, clientSessionID string) string {
	bindingSeed := codexThreadBindingSeed(account, clientSessionID)
	if bindingSeed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, bindingSeed)
}

func codexThreadBindingSeed(account *Account, clientSessionID string) string {
	clientSessionID = strings.TrimSpace(clientSessionID)
	if account == nil || clientSessionID == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return fmt.Sprintf("sub2api:codex-thread-id:v3:%s:%s", seed, clientSessionID)
}

// resolveConvergedCockpitRootIDs 只为两侧均无绑定的新根共享 session/thread UUID。
// 历史双边或单边绑定继续沿用各自命名空间，避免升级时改变既有拓扑。
func resolveConvergedCockpitRootIDs(account *Account, sessionSeed, threadSeed string) (string, string) {
	sessionBindingSeed := codexCockpitSessionBindingSeed(account, sessionSeed)
	threadBindingSeed := codexThreadBindingSeed(account, threadSeed)
	if sessionBindingSeed == "" || threadBindingSeed == "" {
		return "", ""
	}
	if !account.codexIdentityLockHeld {
		lock := codexIdentityBindingLock(account.ID)
		lock.Lock()
		defer lock.Unlock()
	}
	nowMS := time.Now().UnixMilli()
	sessionID, hasSession := loadCodexUUIDv7Binding(account, sessionBindingSeed, CodexIdentityBindingsExtraKey, codexIdentityBindingIdleTTL, codexIdentityBindingMaxEntries, nowMS)
	threadID, hasThread := loadCodexUUIDv7Binding(account, threadBindingSeed, CodexIdentityBindingsExtraKey, codexIdentityBindingIdleTTL, codexIdentityBindingMaxEntries, nowMS)
	switch {
	case hasSession && hasThread:
		return sessionID, threadID
	case hasSession:
		threadID = newCodexUUIDv7().String()
		storeCodexUUIDv7Sibling(account, sessionBindingSeed, threadBindingSeed, CodexIdentityBindingsExtraKey, threadID, codexIdentityBindingIdleTTL, codexIdentityBindingMaxEntries, nowMS)
		return sessionID, threadID
	case hasThread:
		sessionID = newCodexUUIDv7().String()
		storeCodexUUIDv7Sibling(account, threadBindingSeed, sessionBindingSeed, CodexIdentityBindingsExtraKey, sessionID, codexIdentityBindingIdleTTL, codexIdentityBindingMaxEntries, nowMS)
		return sessionID, threadID
	default:
		sharedID := newCodexUUIDv7().String()
		storeCodexUUIDv7Pair(account, sessionBindingSeed, threadBindingSeed, CodexIdentityBindingsExtraKey, sharedID, codexIdentityBindingIdleTTL, codexIdentityBindingMaxEntries, nowMS)
		return sharedID, sharedID
	}
}

// resolveConvergedCockpitTurnID 在账号和根 session 作用域内，
// 为客户端明确提供的回合标识建立稳定 UUIDv7 映射。父子线程共享根 session，
// 因而不能把当前 thread 纳入种子，否则子线程引用的父回合会映射到另一个 UUID。
func resolveConvergedCockpitTurnID(account *Account, sessionID, originalTurnID string) string {
	originalTurnID = strings.TrimSpace(originalTurnID)
	if account == nil || originalTurnID == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	sessionID = strings.TrimSpace(sessionID)
	if seed == "" || sessionID == "" {
		return ""
	}
	lineageSeed := fmt.Sprintf(
		"sub2api:codex-turn-lineage:v2:%s:%d:%s:%d:%s",
		seed,
		len(sessionID), sessionID,
		len(originalTurnID), originalTurnID,
	)
	// 实验模式对字符串与 UUID 原值采用相同的持久化策略；首次生成后
	// 复用完整 UUID，不因原值格式或冷热缓存差异切换派生规则。
	return deriveStableUUIDv7ForAccountStore(
		account,
		lineageSeed,
		CodexTurnLineageBindingsExtraKey,
		codexTurnLineageBindingIdleTTL,
		codexTurnLineageBindingMaxEntries,
	)
}

// resolveCodexParentThreadID 将客户端父线程映射到当前账号的线程命名空间。
// 当前线程本身作为父线程时复用已解析值；full 模式把线程合并到账号线程。
func resolveCodexParentThreadID(account *Account, mode codexFingerprintMode, originalThreadID, threadID, originalParentThreadID string) string {
	originalParentThreadID = strings.TrimSpace(originalParentThreadID)
	if originalParentThreadID == "" {
		return ""
	}
	if originalParentThreadID == strings.TrimSpace(originalThreadID) && strings.TrimSpace(threadID) != "" {
		return strings.TrimSpace(threadID)
	}
	if mode == codexFingerprintFull && strings.TrimSpace(threadID) != "" {
		return strings.TrimSpace(threadID)
	}
	return resolveConvergedThreadID(account, originalParentThreadID)
}

// resolveCockpitTurnLineage 将当前 turn 及其 parent/root 引用放入同一映射图。
// parent/root 只在客户端明确提供时生成；缺失字段始终保持缺失。
func resolveCockpitTurnLineage(account *Account, ids *codexFingerprintIDs) {
	if ids == nil {
		return
	}
	ids.parentThreadID = resolveCodexParentThreadID(account, ids.mode, ids.originalThreadID, ids.threadID, ids.originalParentThreadID)
	// 请求及 WS 连接使用准备阶段冻结的选择；默认路径不查回合映射表。
	if ids.turnMode != codexTurnConverge {
		ids.turnID = ids.originalTurnID
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = ids.originalRootTurnID
		}
		return
	}
	if ids.extendedTurnIdentity {
		// 首次看到一条既有子链时先映射 root/parent，再映射当前 turn，
		// 使新生成 UUIDv7 的时间顺序与引用拓扑一致。
		if ids.originalRootTurnID != "" && ids.originalRootTurnID != ids.originalTurnID {
			ids.rootTurnID = resolveConvergedCockpitTurnID(account, ids.sessionID, ids.originalRootTurnID)
		} else {
			ids.rootTurnID = ""
		}
		ids.parentTurnID = resolveConvergedCockpitTurnID(account, ids.sessionID, ids.originalParentTurnID)
	}
	if ids.originalTurnID != "" {
		ids.turnID = resolveConvergedCockpitTurnID(account, ids.sessionID, ids.originalTurnID)
	}
	if ids.extendedTurnIdentity && ids.originalRootTurnID != "" && ids.originalRootTurnID == ids.originalTurnID {
		ids.rootTurnID = ids.turnID
	}
}

// resolveCodexRootTurnID 仅处理客户端明确提供的根，不因缺少 parent 而补全 root。
// 已有顶层根对齐出站 turn_id，子回合继承已有根；缺失始终保持缺失。
// 旧客户端由外层版本门控处理，不新增扩展回合字段。
func resolveCodexRootTurnID(originalRootTurnID, parentTurnID, turnID string) string {
	originalRootTurnID = strings.TrimSpace(originalRootTurnID)
	parentTurnID = strings.TrimSpace(parentTurnID)
	turnID = strings.TrimSpace(turnID)
	if originalRootTurnID == "" {
		return ""
	}
	if parentTurnID == "" {
		return turnID
	}
	return originalRootTurnID
}

// resolveOfficialCockpitPromptCacheKey 对齐 Codex 默认规则：显式缓存键原样保留，
// 缺省时使用根 session_id；不再为缓存键额外构造一个带伪时间戳的 UUIDv7。
func resolveOfficialCockpitPromptCacheKey(sessionID, promptCacheKey string) string {
	if promptCacheKey != "" {
		return promptCacheKey
	}
	return strings.TrimSpace(sessionID)
}

// resolveCockpitSessionSeed chooses the stable server-side session seed.
// Session markers are preferred so a client-side prompt_cache_key rotation
// (for example after compaction) does not split the server session binding.
func resolveCockpitSessionSeed(source codexFingerprintSource) string {
	return firstNonEmptyCodexValue(
		source.clientSessionID,
		source.originalSessionID,
		source.threadID,
		source.promptCacheKey,
	)
}

// resolveCockpitThreadSeed keeps thread identity tied to the client's thread
// marker when present, while still falling back to the stable session marker.
// The explicit prompt_cache_key is only a last-resort seed and never replaces
// the client-owned cache key in the forwarded request.
func resolveCockpitThreadSeed(source codexFingerprintSource) string {
	return firstNonEmptyCodexValue(
		source.threadID,
		source.clientSessionID,
		source.originalSessionID,
		source.promptCacheKey,
	)
}

// isExplicitCodexRootThread 仅接受客户端同时给出的同值 session/thread。
// 缺少 thread 时仍走旧兼容派生，避免把服务端回退值误判为官方根拓扑。
func isExplicitCodexRootThread(source codexFingerprintSource, ids *codexFingerprintIDs) bool {
	if ids == nil || strings.TrimSpace(source.threadID) == "" || strings.TrimSpace(source.parentThreadID) != "" {
		return false
	}
	return ids.originalSessionID != "" && ids.originalSessionID == ids.originalThreadID
}

// codexFingerprintSource 保存客户端原始身份字段，供不同模式选择派生种子。
type codexFingerprintSource struct {
	clientVersion        string
	installationID       string
	clientSessionID      string
	originalSessionID    string
	threadID             string
	parentThreadID       string
	turnID               string
	parentTurnID         string
	rootTurnID           string
	turnStartedAtUnixMS  int64
	turnStartedAtPresent bool
	windowID             string
	windowNumber         uint64
	windowNumberPresent  bool
	firstWindowID        string
	previousWindowID     string
	contextWindowID      string
	promptCacheKey       string
	// promptCacheKeyPresent 区分客户端明确发送空字符串与字段缺失。
	promptCacheKeyPresent bool
	promptCacheKeyInBody  bool
	allowPromptCacheCarry bool
}

// codexFingerprintIDs 保存身份快照和本次选定的回合策略。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 及父子引用一致。
type codexFingerprintIDs struct {
	// stagedAccountID 记录本次请求实际调度的账号。Spark 影子的身份字段由父账号
	// 派生，但暂存值只能由同一个影子尝试读取，避免 OAuth→OAuth failover 误用。
	stagedAccountID        int64
	stagedAccountBound     bool
	mode                   codexFingerprintMode
	turnMode               codexTurnMode
	extendedTurnIdentity   bool
	originalInstallationID string
	installationID         string
	originalSessionID      string
	sessionID              string
	originalThreadID       string
	threadID               string
	originalParentThreadID string
	parentThreadID         string
	originalTurnID         string
	turnID                 string
	// turnIDPresent 表示当前请求或 WS 帧明确携带有效 turn_id；Cockpit 不回灌历史值。
	turnIDPresent           bool
	originalParentTurnID    string
	parentTurnID            string
	originalRootTurnID      string
	rootTurnID              string
	originalContextWindowID string
	contextWindowID         string
	// windowInstanceID 仅保存内部当前实例，不代表本帧有权写出可选字段。
	windowInstanceID    string
	turnStartedAtUnixMS int64
	// turnStartedAtPresent 区分客户端明确提供的开始时间与字段缺失。
	turnStartedAtPresent bool
	originalWindowID     string
	windowID             string
	windowNumber         uint64
	// windowNumberPresent 将内部窗口代数与客户端实际发送的字段分开。
	windowNumberPresent      bool
	originalFirstWindowID    string
	firstWindowID            string
	originalPreviousWindowID string
	previousWindowID         string
	originalPromptCacheKey   string
	promptCacheKey           string
	// promptCacheKeyPresent 保留当前请求是否明确携带缓存键，避免空值误走 carry。
	promptCacheKeyPresent bool
	// promptCacheKeyInBody 区分原请求体字段与仅用于 Header 的兼容缓存键。
	promptCacheKeyInBody bool
}

// bindCodexFingerprintIDsToAccount 将派生结果绑定到本次实际调度账号。
// 身份可以来自 OAuth 父账号，但 context 暂存值的所有权必须属于当前调度尝试。
func bindCodexFingerprintIDsToAccount(ids *codexFingerprintIDs, account *Account) *codexFingerprintIDs {
	if ids != nil && account != nil {
		ids.stagedAccountID = account.ID
		ids.stagedAccountBound = true
	}
	return ids
}

// resolveCodexFingerprintIDs 按收敛模式计算出站 ID 集合。
// clientSessionID 是客户端原始的 session-id 头值（连字符形式），用于 session 模式下
// 的 thread_id 派生——每个真实 Codex 会话得到一个独立线程。
// 返回 nil 表示 off 模式，不需要改写。
// 调用方只准备一次快照并共享给头/体改写；透传模式直接保留客户端回合。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsWithSource(account, codexFingerprintSource{clientSessionID: clientSessionID}, mode)
}

// resolveCodexFingerprintIDsWithSource 使用完整客户端身份来源计算出站 ID 集合。
func resolveCodexFingerprintIDsWithSource(account *Account, source codexFingerprintSource, mode codexFingerprintMode) *codexFingerprintIDs {
	if mode == codexFingerprintOff {
		return nil
	}
	// 兼容内部测试和历史调用方直接填充非空缓存键的来源结构。
	if !source.promptCacheKeyPresent && source.promptCacheKey != "" {
		source.promptCacheKeyPresent = true
	}

	ids := &codexFingerprintIDs{
		mode:                     mode,
		turnMode:                 account.GetCodexTurnMode(),
		extendedTurnIdentity:     codexSupportsExtendedTurnIdentity(source.clientVersion),
		originalInstallationID:   strings.TrimSpace(source.installationID),
		originalSessionID:        strings.TrimSpace(source.originalSessionID),
		originalThreadID:         strings.TrimSpace(source.threadID),
		originalParentThreadID:   strings.TrimSpace(source.parentThreadID),
		originalTurnID:           strings.TrimSpace(source.turnID),
		turnIDPresent:            strings.TrimSpace(source.turnID) != "",
		originalParentTurnID:     strings.TrimSpace(source.parentTurnID),
		originalRootTurnID:       strings.TrimSpace(source.rootTurnID),
		originalContextWindowID:  strings.TrimSpace(source.contextWindowID),
		originalWindowID:         strings.TrimSpace(source.windowID),
		windowNumberPresent:      source.windowNumberPresent,
		originalFirstWindowID:    strings.TrimSpace(source.firstWindowID),
		originalPreviousWindowID: strings.TrimSpace(source.previousWindowID),
		originalPromptCacheKey:   source.promptCacheKey,
		promptCacheKeyPresent:    source.promptCacheKeyPresent,
		turnStartedAtPresent:     source.turnStartedAtPresent,
	}
	if ids.originalSessionID == "" {
		ids.originalSessionID = strings.TrimSpace(source.clientSessionID)
	}
	if ids.originalThreadID == "" {
		ids.originalThreadID = ids.originalSessionID
	}

	ids.installationID = resolveConvergedInstallationID(account)
	if ids.installationID == "" {
		return nil
	}

	switch mode {
	case codexFingerprintDevice:
		return ids

	case codexFingerprintSession:
		ids.sessionID = resolveConvergedSessionID(account)
		if ids.sessionID == "" {
			return nil
		}
		ids.threadID = resolveConvergedThreadID(account, source.clientSessionID)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}
		ids.parentThreadID = resolveCodexParentThreadID(account, ids.mode, ids.originalThreadID, ids.threadID, ids.originalParentThreadID)
		ids.turnID = newCodexUUIDv7().String()
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = resolveCodexRootTurnID(ids.originalRootTurnID, ids.originalParentTurnID, ids.turnID)
		}
		resolveCodexFingerprintWindow(account, source, ids)
		ids.turnStartedAtUnixMS = resolveCodexTurnStartedAt(account, ids.mode, ids.sessionID, ids.originalTurnID, ids.turnID, source.turnStartedAtUnixMS, source.turnStartedAtPresent)
		return ids

	case codexFingerprintCockpit:
		// Cockpit keeps device and conversation identity server-derived. Client
		// conversation fields are used only as a stable seed; the explicit
		// prompt_cache_key remains the one client-controlled cache identity.
		sessionSeed := resolveCockpitSessionSeed(source)
		threadSeed := resolveCockpitThreadSeed(source)
		if isExplicitCodexRootThread(source, ids) {
			ids.sessionID, ids.threadID = resolveConvergedCockpitRootIDs(account, sessionSeed, threadSeed)
		} else {
			ids.sessionID = resolveConvergedCockpitSessionID(account, sessionSeed)
			ids.threadID = resolveConvergedThreadID(account, threadSeed)
		}
		if ids.sessionID == "" {
			ids.sessionID = resolveConvergedSessionID(account)
		}
		if ids.sessionID == "" {
			return nil
		}
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}

		resolveCockpitTurnLineage(account, ids)
		resolveCodexFingerprintWindow(account, source, ids)
		if source.promptCacheKeyPresent {
			ids.promptCacheKey = source.promptCacheKey
			// 旧 compact 的 Header-only 临时键不属于普通 Responses 缓存绑定。
			// 禁止其覆盖此前明确的 Body 键及载体，读写遵守同一入口门控。
			if source.allowPromptCacheCarry && source.promptCacheKey != "" {
				rememberCodexPromptCacheKey(account, ids, ids.promptCacheKey, source.promptCacheKeyInBody)
			}
		} else if source.allowPromptCacheCarry {
			if carried, ok := loadCodexPromptCacheKey(account, ids); ok {
				ids.promptCacheKey = carried.Key
				ids.promptCacheKeyInBody = carried.InBody
			} else {
				ids.promptCacheKey = resolveOfficialCockpitPromptCacheKey(ids.sessionID, "")
			}
		} else {
			ids.promptCacheKey = resolveOfficialCockpitPromptCacheKey(ids.sessionID, "")
		}
		// Cockpit 的回合开始时间属于当前客户端载荷；缺失时不把本地缓存值回灌。
		ids.turnStartedAtUnixMS = source.turnStartedAtUnixMS
		// 显式键保持原载体；短暂复用时沿用上一次键的载体形态。
		if source.promptCacheKeyPresent || !source.allowPromptCacheCarry {
			ids.promptCacheKeyInBody = source.promptCacheKeyInBody
		}
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(account)
		if ids.sessionID == "" {
			return nil
		}
		ids.threadID = ids.sessionID
		ids.parentThreadID = resolveCodexParentThreadID(account, ids.mode, ids.originalThreadID, ids.threadID, ids.originalParentThreadID)
		ids.turnID = newCodexUUIDv7().String()
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = resolveCodexRootTurnID(ids.originalRootTurnID, ids.originalParentTurnID, ids.turnID)
		}
		resolveCodexFingerprintWindow(account, source, ids)
		ids.turnStartedAtUnixMS = resolveCodexTurnStartedAt(account, ids.mode, ids.sessionID, ids.originalTurnID, ids.turnID, source.turnStartedAtUnixMS, source.turnStartedAtPresent)
		return ids
	}

	return nil
}

// resolveCodexFingerprintWindow 统一三种模式的窗口派生及版本门控，
// 旧客户端已剥离的 window_number 不得再改变窗口身份或缓存绑定。
func resolveCodexFingerprintWindow(account *Account, source codexFingerprintSource, ids *codexFingerprintIDs) {
	windowSource := source.windowID
	if ids.extendedTurnIdentity && source.windowNumberPresent {
		windowSource = fmt.Sprintf("%s:%d", ids.threadID, source.windowNumber)
	}
	ids.windowID = normalizeCodexWindowID(windowSource, ids.threadID)
	if ids.extendedTurnIdentity {
		ids.windowInstanceID = ""
		if ids.mode == codexFingerprintCockpit {
			ids.windowInstanceID = strings.TrimSpace(source.contextWindowID)
		}
		if ids.windowInstanceID != "" {
			ids.windowNumber = codexWindowGeneration(ids.windowID)
			ids.contextWindowID = resolveCodexWindowInstance(account, ids.threadID, ids.windowInstanceID)
			ids.firstWindowID, ids.previousWindowID = "", ""
		} else {
			ids.contextWindowID = resolveConvergedContextWindowID(account, ids.threadID, ids.windowID)
			ids.windowNumber, ids.firstWindowID, ids.previousWindowID = resolveCodexWindowLineage(account, ids.threadID, ids.windowID)
		}
		if ids.mode == codexFingerprintCockpit {
			if source.firstWindowID != "" {
				ids.firstWindowID = resolveCodexWindowInstance(account, ids.threadID, source.firstWindowID)
			}
			if source.previousWindowID != "" && ids.windowNumber > 0 {
				ids.previousWindowID = resolveCodexWindowInstance(account, ids.threadID, source.previousWindowID)
			}
		}
	}
}

// shouldWriteCodexTurnID 保留 session/full 的既有兼容行为；Cockpit 仅改写客户端字段。
func shouldWriteCodexTurnID(ids *codexFingerprintIDs) bool {
	return ids != nil && ids.turnID != "" && (ids.mode != codexFingerprintCockpit || ids.turnIDPresent)
}

// shouldWriteCodexTurnStartedAt 只在当前载荷明确携带开始时间时写出。
// Cockpit 缺失字段保持缺失，避免网关制造新的生命周期特征。
func shouldWriteCodexTurnStartedAt(ids *codexFingerprintIDs) bool {
	if ids == nil {
		return false
	}
	if ids.mode == codexFingerprintCockpit {
		return ids.turnStartedAtPresent
	}
	return ids.turnID != ""
}

// shouldWriteCodexWindowNumber 允许内部维护窗口链，但不把缺失的压缩次数补入 Cockpit 请求。
func shouldWriteCodexWindowNumber(ids *codexFingerprintIDs) bool {
	return ids != nil && ids.extendedTurnIdentity && (ids.mode != codexFingerprintCockpit || ids.windowNumberPresent)
}

// Cockpit 只改写客户端实际携带的可选窗口字段。first/previous 属于兼容扩展，
// context_window_id 虽是官方 turn metadata，也不应替缺少该字段的兼容客户端补造状态。
func shouldWriteCodexContextWindowID(ids *codexFingerprintIDs) bool {
	return ids != nil && ids.contextWindowID != "" && (ids.mode != codexFingerprintCockpit || ids.originalContextWindowID != "")
}

func shouldWriteCodexFirstWindowID(ids *codexFingerprintIDs) bool {
	return ids != nil && ids.firstWindowID != "" && (ids.mode != codexFingerprintCockpit || ids.originalFirstWindowID != "")
}

func shouldManageCodexPreviousWindowID(ids *codexFingerprintIDs) bool {
	return ids != nil && ids.extendedTurnIdentity && (ids.mode != codexFingerprintCockpit || ids.originalPreviousWindowID != "")
}

// extractCodexStringField 读取 map 中的非空字符串字段。
func extractCodexStringField(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

// extractCodexRawStringField 读取字符串字段但保留首尾空白。
// 只用于客户端拥有字面值语义的 prompt_cache_key 等字段。
func extractCodexRawStringField(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key].(string)
	return value, ok
}

// extractCodexWindowNumberField 兼容 JSON 数字与字符串形式；0 是合法首窗口，
// 因而额外返回存在性。统一限制在 JSON float64 精确整数范围，防止两条路径分叉。
func extractCodexWindowNumberField(values map[string]any, key string) (uint64, bool) {
	if values == nil {
		return 0, false
	}
	switch value := values[key].(type) {
	case json.Number:
		n, err := strconv.ParseFloat(string(value), 64)
		if err == nil {
			return extractCodexWindowNumberField(map[string]any{key: n}, key)
		}
	case float64:
		// 解码体使用 float64，超出精确范围的数字不得影响窗口身份。
		const maxExactJSONInteger = float64(1<<53 - 1)
		if value >= 0 && value == math.Trunc(value) && value <= maxExactJSONInteger {
			return uint64(value), true
		}
	case int:
		if value >= 0 && uint64(value) <= 1<<53-1 {
			return uint64(value), true
		}
	case int64:
		if value >= 0 && uint64(value) <= 1<<53-1 {
			return uint64(value), true
		}
	case uint64:
		if value <= 1<<53-1 {
			return value, true
		}
	case string:
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err == nil && n <= 1<<53-1 {
			return n, true
		}
	}
	return 0, false
}

func extractCodexWindowNumberRaw(body []byte, path string) (uint64, bool) {
	result := gjson.GetBytes(body, path)
	if !result.Exists() || result.Type == gjson.Null {
		return 0, false
	}
	if result.Type == gjson.Number {
		return extractCodexWindowNumberField(map[string]any{"value": json.Number(result.Raw)}, "value")
	}
	return extractCodexWindowNumberField(map[string]any{"value": result.String()}, "value")
}

func extractCodexTurnStartedAtField(values map[string]any, key string) (int64, bool) {
	value, ok := extractCodexWindowNumberField(values, key)
	if !ok || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

func extractCodexTurnStartedAtRaw(body []byte, path string) (int64, bool) {
	value, ok := extractCodexWindowNumberRaw(body, path)
	if !ok || value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

// extractCodexTurnMetadataField 从 JSON 字符串形式的回合元数据读取身份字段。
func extractCodexTurnMetadataField(raw, key string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	result := gjson.Get(raw, key)
	if result.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(result.Str)
}

func extractCodexTurnMetadataRawStringField(raw, key string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	result := gjson.Get(raw, key)
	if result.Type != gjson.String {
		return "", false
	}
	return result.Str, true
}

// firstNonEmptyCodexValue 返回首个非空身份字段。
func firstNonEmptyCodexValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// extractCockpitFingerprintSource 按 Cockpit 的兼容顺序从头和请求体提取身份来源。
func extractCockpitFingerprintSource(h http.Header, reqBody map[string]any) codexFingerprintSource {
	source := codexFingerprintSource{
		clientSessionID: extractClientSessionID(h),
		clientVersion:   codexClientVersionFromHeaders(h),
	}
	clientMetadata, _ := reqBody["client_metadata"].(map[string]any)
	embeddedTurnMetadata := extractCodexStringField(clientMetadata, "x-codex-turn-metadata")
	headerTurnMetadata := ""
	if h != nil {
		headerTurnMetadata = h.Get("x-codex-turn-metadata")
	}
	source.installationID = firstNonEmptyCodexValue(
		h.Get("x-codex-installation-id"),
		extractCodexStringField(clientMetadata, "x-codex-installation-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "installation_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "installation_id"),
	)
	source.originalSessionID = firstNonEmptyCodexValue(
		source.clientSessionID,
		extractCodexStringField(reqBody, "session_id"),
		extractCodexStringField(reqBody, "session-id"),
		extractCodexStringField(clientMetadata, "session_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "session_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "session_id"),
	)

	if source.clientSessionID == "" {
		for _, value := range []string{
			extractCodexStringField(reqBody, "session_id"),
			extractCodexStringField(reqBody, "session-id"),
			extractCodexStringField(clientMetadata, "session_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "session_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "session_id"),
			codexSessionSeedFromWindowID(extractCodexStringField(clientMetadata, "x-codex-window-id")),
			extractCodexStringField(reqBody, "prompt_cache_key"),
		} {
			if strings.TrimSpace(value) != "" {
				source.clientSessionID = strings.TrimSuffix(strings.TrimSpace(value), ":0")
				break
			}
		}
	}

	source.threadID = extractCodexStringField(clientMetadata, "thread_id")
	if source.threadID == "" {
		source.threadID = firstNonEmptyCodexValue(
			extractCodexStringField(reqBody, "thread_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "thread_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "thread_id"),
			h.Get("thread-id"),
		)
	}
	source.parentThreadID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "parent_thread_id"),
		extractCodexStringField(clientMetadata, "parent_thread_id"),
		extractCodexStringField(clientMetadata, "x-codex-parent-thread-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "parent_thread_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "x-codex-parent-thread-id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "parent_thread_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "x-codex-parent-thread-id"),
		h.Get("x-codex-parent-thread-id"),
	)
	source.turnID = firstNonEmptyCodexValue(
		extractCodexStringField(clientMetadata, "turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "turn_id"),
	)
	source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtField(reqBody, "turn_started_at_unix_ms")
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtField(clientMetadata, "turn_started_at_unix_ms")
	}
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw([]byte(embeddedTurnMetadata), "turn_started_at_unix_ms")
	}
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw([]byte(headerTurnMetadata), "turn_started_at_unix_ms")
	}
	source.parentTurnID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "parent_turn_id"),
		extractCodexStringField(clientMetadata, "parent_turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "parent_turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "parent_turn_id"),
		h.Get("x-codex-parent-turn-id"),
	)
	source.rootTurnID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "root_turn_id"),
		extractCodexStringField(clientMetadata, "root_turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "root_turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "root_turn_id"),
		h.Get("x-codex-root-turn-id"),
	)
	source.windowID = firstNonEmptyCodexValue(
		extractCodexStringField(clientMetadata, "x-codex-window-id"),
		extractCodexStringField(reqBody, "window_id"),
		h.Get("x-codex-window-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "window_id"),
	)
	source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberField(reqBody, "window_number")
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberField(clientMetadata, "window_number")
	}
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw([]byte(embeddedTurnMetadata), "window_number")
	}
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw([]byte(headerTurnMetadata), "window_number")
	}
	source.firstWindowID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "first_window_id"),
		extractCodexStringField(clientMetadata, "first_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "first_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "first_window_id"),
	)
	source.previousWindowID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "previous_window_id"),
		extractCodexStringField(clientMetadata, "previous_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "previous_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "previous_window_id"),
	)
	source.contextWindowID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "context_window_id"),
		extractCodexStringField(clientMetadata, "context_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "context_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "context_window_id"),
		h.Get("x-codex-context-window-id"),
	)
	source.promptCacheKey, source.promptCacheKeyInBody = extractCodexRawStringField(reqBody, "prompt_cache_key")
	source.promptCacheKeyPresent = source.promptCacheKeyInBody
	if !source.promptCacheKeyPresent {
		if value, ok := extractCodexTurnMetadataRawStringField(embeddedTurnMetadata, "prompt_cache_key"); ok {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		} else if value, ok := extractCodexTurnMetadataRawStringField(headerTurnMetadata, "prompt_cache_key"); ok {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		} else if value, ok := codexHeaderRawValue(h, "conversation_id"); ok && value != "" {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		}
	}
	// x-client-request-id is often a per-request UUID. Keep it as the final
	// fallback so it cannot override a stable thread/session/cache identity.
	if source.threadID == "" && source.promptCacheKey == "" && source.originalSessionID == "" {
		source.threadID = strings.TrimSpace(h.Get("x-client-request-id"))
	}
	return source
}

// extractCockpitFingerprintSourceRaw 从原始 JSON 中局部读取 Cockpit 身份字段，
// 避免 OAuth 透传路径为大请求体做整包反序列化。
func extractCockpitFingerprintSourceRaw(h http.Header, body []byte) codexFingerprintSource {
	source := codexFingerprintSource{
		clientSessionID: extractClientSessionID(h),
		clientVersion:   codexClientVersionFromHeaders(h),
	}
	read := func(path string) string {
		result := gjson.GetBytes(body, path)
		if result.Type != gjson.String {
			return ""
		}
		return strings.TrimSpace(result.Str)
	}
	readRaw := func(path string) (string, bool) {
		result := gjson.GetBytes(body, path)
		if result.Type != gjson.String {
			return "", false
		}
		return result.Str, true
	}
	embeddedTurnMetadata := read("client_metadata.x-codex-turn-metadata")
	headerTurnMetadata := ""
	if h != nil {
		headerTurnMetadata = h.Get("x-codex-turn-metadata")
	}
	source.installationID = firstNonEmptyCodexValue(
		h.Get("x-codex-installation-id"),
		read("client_metadata.x-codex-installation-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "installation_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "installation_id"),
	)
	source.originalSessionID = firstNonEmptyCodexValue(
		source.clientSessionID,
		read("session_id"),
		read("session-id"),
		read("client_metadata.session_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "session_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "session_id"),
	)

	if source.clientSessionID == "" {
		for _, value := range []string{
			read("session_id"),
			read("session-id"),
			read("client_metadata.session_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "session_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "session_id"),
			codexSessionSeedFromWindowID(read("client_metadata.x-codex-window-id")),
			read("prompt_cache_key"),
		} {
			if strings.TrimSpace(value) != "" {
				source.clientSessionID = strings.TrimSuffix(strings.TrimSpace(value), ":0")
				break
			}
		}
	}

	source.threadID = read("client_metadata.thread_id")
	if source.threadID == "" {
		source.threadID = firstNonEmptyCodexValue(
			read("thread_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "thread_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "thread_id"),
			h.Get("thread-id"),
		)
	}
	source.parentThreadID = firstNonEmptyCodexValue(
		read("parent_thread_id"),
		read("client_metadata.parent_thread_id"),
		read("client_metadata.x-codex-parent-thread-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "parent_thread_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "x-codex-parent-thread-id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "parent_thread_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "x-codex-parent-thread-id"),
		h.Get("x-codex-parent-thread-id"),
	)
	source.turnID = firstNonEmptyCodexValue(
		read("client_metadata.turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "turn_id"),
	)
	source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw(body, "turn_started_at_unix_ms")
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw(body, "client_metadata.turn_started_at_unix_ms")
	}
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw([]byte(embeddedTurnMetadata), "turn_started_at_unix_ms")
	}
	if !source.turnStartedAtPresent {
		source.turnStartedAtUnixMS, source.turnStartedAtPresent = extractCodexTurnStartedAtRaw([]byte(headerTurnMetadata), "turn_started_at_unix_ms")
	}
	source.parentTurnID = firstNonEmptyCodexValue(
		read("parent_turn_id"),
		read("client_metadata.parent_turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "parent_turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "parent_turn_id"),
		h.Get("x-codex-parent-turn-id"),
	)
	source.rootTurnID = firstNonEmptyCodexValue(
		read("root_turn_id"),
		read("client_metadata.root_turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "root_turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "root_turn_id"),
		h.Get("x-codex-root-turn-id"),
	)
	source.windowID = firstNonEmptyCodexValue(
		read("client_metadata.x-codex-window-id"),
		read("window_id"),
		h.Get("x-codex-window-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "window_id"),
	)
	source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw(body, "window_number")
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw(body, "client_metadata.window_number")
	}
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw([]byte(embeddedTurnMetadata), "window_number")
	}
	if !source.windowNumberPresent {
		source.windowNumber, source.windowNumberPresent = extractCodexWindowNumberRaw([]byte(headerTurnMetadata), "window_number")
	}
	source.firstWindowID = firstNonEmptyCodexValue(
		read("first_window_id"),
		read("client_metadata.first_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "first_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "first_window_id"),
	)
	source.previousWindowID = firstNonEmptyCodexValue(
		read("previous_window_id"),
		read("client_metadata.previous_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "previous_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "previous_window_id"),
	)
	source.contextWindowID = firstNonEmptyCodexValue(
		read("context_window_id"),
		read("client_metadata.context_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "context_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "context_window_id"),
		h.Get("x-codex-context-window-id"),
	)
	source.promptCacheKey, source.promptCacheKeyInBody = readRaw("prompt_cache_key")
	source.promptCacheKeyPresent = source.promptCacheKeyInBody
	if !source.promptCacheKeyPresent {
		if value, ok := extractCodexTurnMetadataRawStringField(embeddedTurnMetadata, "prompt_cache_key"); ok {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		} else if value, ok := extractCodexTurnMetadataRawStringField(headerTurnMetadata, "prompt_cache_key"); ok {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		} else if value, ok := codexHeaderRawValue(h, "conversation_id"); ok && value != "" {
			source.promptCacheKey, source.promptCacheKeyPresent = value, true
		}
	}
	if source.threadID == "" && source.promptCacheKey == "" && source.originalSessionID == "" {
		source.threadID = strings.TrimSpace(h.Get("x-client-request-id"))
	}
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

// codexHeaderRawValue 返回 Header 中是否真正存在该键，保留空字符串的存在性。
func codexHeaderRawValue(h http.Header, key string) (string, bool) {
	if h == nil {
		return "", false
	}
	for headerKey, values := range h {
		if strings.EqualFold(headerKey, key) {
			if len(values) == 0 {
				return "", true
			}
			return values[0], true
		}
	}
	return "", false
}

// resolveCodexFingerprintIDsFromRequest 从客户端原始请求头中提取 session-id，
// 结合账号配置一次性解析收敛 ID 集合。调用方应将返回的 ids 同时传给
// applyCodexFingerprintHeaders 和 applyCodexFingerprintClientMetadata。
func resolveCodexFingerprintIDsFromRequest(account *Account, clientHeaders http.Header, reqBodies ...map[string]any) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsFromRequestWithCarry(account, clientHeaders, true, reqBodies...)
}

func resolveCodexFingerprintIDsFromRequestWithCarry(account *Account, clientHeaders http.Header, allowPromptCacheCarry bool, reqBodies ...map[string]any) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	var reqBody map[string]any
	if len(reqBodies) > 0 {
		reqBody = reqBodies[0]
	}
	source := extractCockpitFingerprintSource(clientHeaders, reqBody)
	source.allowPromptCacheCarry = allowPromptCacheCarry
	if mode != codexFingerprintCockpit {
		// session 模式继续只使用标准请求头派生线程，避免改变既有拓扑。
		source.clientSessionID = extractClientSessionID(clientHeaders)
	}
	return resolveCodexFingerprintIDsWithSource(account, source, mode)
}

// resolveCodexFingerprintIDsFromRawRequest 为透传路径从原始请求体提取 Cockpit 会话来源。
func resolveCodexFingerprintIDsFromRawRequest(account *Account, clientHeaders http.Header, body []byte, allowPromptCacheCarry ...bool) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.GetCodexFingerprintMode()
	if mode == codexFingerprintOff {
		return nil
	}
	source := extractCockpitFingerprintSourceRaw(clientHeaders, body)
	source.allowPromptCacheCarry = len(allowPromptCacheCarry) == 0 || allowPromptCacheCarry[0]
	if mode != codexFingerprintCockpit {
		source.clientSessionID = extractClientSessionID(clientHeaders)
	}
	return resolveCodexFingerprintIDsWithSource(account, source, mode)
}

// codexFingerprintResponseMappings 返回收敛值到客户端原始值的完整映射。
func codexFingerprintResponseMappings(ids *codexFingerprintIDs) [][2]string {
	return [][2]string{
		{ids.windowID, ids.originalWindowID},
		{ids.firstWindowID, ids.originalFirstWindowID},
		{ids.previousWindowID, ids.originalPreviousWindowID},
		{ids.contextWindowID, ids.originalContextWindowID},
		{ids.promptCacheKey, ids.originalPromptCacheKey},
		{ids.turnID, ids.originalTurnID},
		{ids.parentThreadID, ids.originalParentThreadID},
		{ids.parentTurnID, ids.originalParentTurnID},
		{ids.rootTurnID, ids.originalRootTurnID},
		{ids.installationID, ids.originalInstallationID},
		{ids.sessionID, ids.originalSessionID},
		{ids.threadID, ids.originalThreadID},
	}
}

// restoreCodexFingerprintFieldValue 按 JSON 字段语义恢复身份。
// full 模式的 sessionID 与 threadID 相同，必须依靠字段名区分两个原始值。
func restoreCodexFingerprintFieldValue(field, value string, ids *codexFingerprintIDs) (string, bool) {
	field = strings.ToLower(strings.TrimSpace(field))
	var from, to string
	switch field {
	case "installation_id", "installation-id", "x-codex-installation-id":
		from, to = ids.installationID, ids.originalInstallationID
	case "session_id", "session-id":
		from, to = ids.sessionID, ids.originalSessionID
	case "thread_id", "thread-id", "x-client-request-id":
		from, to = ids.threadID, ids.originalThreadID
	case "parent_thread_id", "parent-thread-id", "x-codex-parent-thread-id":
		from, to = ids.parentThreadID, ids.originalParentThreadID
	case "turn_id", "turn-id":
		from, to = ids.turnID, ids.originalTurnID
	case "parent_turn_id", "parent-turn-id", "x-codex-parent-turn-id":
		from, to = ids.parentTurnID, ids.originalParentTurnID
	case "root_turn_id", "root-turn-id", "x-codex-root-turn-id":
		from, to = ids.rootTurnID, ids.originalRootTurnID
	case "window_id", "window-id", "x-codex-window-id":
		from, to = ids.windowID, ids.originalWindowID
	case "context_window_id", "context-window-id", "x-codex-context-window-id":
		from, to = ids.contextWindowID, ids.originalContextWindowID
	case "first_window_id", "first-window-id":
		from, to = ids.firstWindowID, ids.originalFirstWindowID
	case "previous_window_id", "previous-window-id":
		from, to = ids.previousWindowID, ids.originalPreviousWindowID
	case "prompt_cache_key", "prompt-cache-key", "conversation_id":
		from, to = ids.promptCacheKey, ids.originalPromptCacheKey
	default:
		return "", false
	}
	cacheKeyField := field == "prompt_cache_key" || field == "prompt-cache-key" || field == "conversation_id"
	if !cacheKeyField {
		from = strings.TrimSpace(from)
		to = strings.TrimSpace(to)
	}
	if from == "" || to == "" || from == to || value != from {
		return "", false
	}
	return to, true
}

// restoreUnambiguousCodexFingerprintValue 恢复没有一对多冲突的精确字符串值。
// 对 full 模式共享的 session/thread 收敛值保持原样，交给字段感知逻辑处理。
func restoreUnambiguousCodexFingerprintValue(value string, ids *codexFingerprintIDs) (string, bool) {
	restored := ""
	found := false
	for _, pair := range codexFingerprintResponseMappings(ids) {
		from, to := pair[0], pair[1]
		if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" || from == to || from != value {
			continue
		}
		if found && restored != to {
			return "", false
		}
		restored = to
		found = true
	}
	return restored, found
}

// restoreCodexFingerprintJSONValue 递归恢复 JSON 对象和数组中的身份字段。
func restoreCodexFingerprintJSONValue(value any, field string, ids *codexFingerprintIDs) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		modified := false
		for key, child := range typed {
			restored, changed := restoreCodexFingerprintJSONValue(child, key, ids)
			if changed {
				typed[key] = restored
				modified = true
			}
		}
		return typed, modified
	case []any:
		modified := false
		for index, child := range typed {
			restored, changed := restoreCodexFingerprintJSONValue(child, field, ids)
			if changed {
				typed[index] = restored
				modified = true
			}
		}
		return typed, modified
	case string:
		if strings.EqualFold(strings.TrimSpace(field), "x-codex-turn-metadata") {
			if restored, changed := restoreCodexFingerprintJSONPayload([]byte(typed), ids); changed {
				return string(restored), true
			}
		}
		if restored, ok := restoreCodexFingerprintFieldValue(field, typed, ids); ok {
			return restored, true
		}
		if restored, ok := restoreUnambiguousCodexFingerprintValue(typed, ids); ok {
			return restored, true
		}
	}
	return value, false
}

// restoreCodexFingerprintJSONPayload 恢复单个 JSON 文档，返回是否成功解析并发生修改。
func restoreCodexFingerprintJSONPayload(payload []byte, ids *codexFingerprintIDs) ([]byte, bool) {
	if !json.Valid(payload) {
		return payload, false
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return payload, false
	}
	restored, modified := restoreCodexFingerprintJSONValue(decoded, "", ids)
	if !modified {
		return payload, false
	}
	rebuilt, err := json.Marshal(restored)
	if err != nil {
		return payload, false
	}
	return rebuilt, true
}

// restoreCodexFingerprintSSEPayload 兼容测试、错误路径或聚合器传入的完整 SSE 文本。
func restoreCodexFingerprintSSEPayload(payload []byte, ids *codexFingerprintIDs) ([]byte, bool) {
	lines := bytes.SplitAfter(payload, []byte("\n"))
	modified := false
	for index, line := range lines {
		lineEnd := []byte{}
		content := line
		if bytes.HasSuffix(content, []byte("\n")) {
			lineEnd = []byte("\n")
			content = content[:len(content)-1]
		}
		if bytes.HasSuffix(content, []byte("\r")) {
			lineEnd = append([]byte("\r"), lineEnd...)
			content = content[:len(content)-1]
		}
		trimmedLeft := bytes.TrimLeft(content, " \t")
		if !bytes.HasPrefix(trimmedLeft, []byte("data:")) {
			continue
		}
		colon := bytes.Index(content, []byte(":"))
		if colon < 0 {
			continue
		}
		jsonStart := colon + 1
		for jsonStart < len(content) && (content[jsonStart] == ' ' || content[jsonStart] == '\t') {
			jsonStart++
		}
		restored, changed := restoreCodexFingerprintJSONPayload(content[jsonStart:], ids)
		if !changed {
			continue
		}
		lines[index] = append(append(append([]byte{}, content[:jsonStart]...), restored...), lineEnd...)
		modified = true
	}
	if !modified {
		return payload, false
	}
	return bytes.Join(lines, nil), true
}

// restoreUnambiguousCodexFingerprintPayload 为非 JSON 错误体保留原有的文本恢复能力。
// 同一收敛值对应多个原始值时跳过，防止 full 模式混淆 session/thread。
func restoreUnambiguousCodexFingerprintPayload(payload []byte, ids *codexFingerprintIDs) []byte {
	mappings := codexFingerprintResponseMappings(ids)
	for index, pair := range mappings {
		from, to := pair[0], pair[1]
		if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" || from == to || !bytes.Contains(payload, []byte(from)) {
			continue
		}
		ambiguous := false
		for otherIndex, other := range mappings {
			if otherIndex == index || other[0] != from {
				continue
			}
			otherTarget := other[1]
			if strings.TrimSpace(otherTarget) != "" && otherTarget != to {
				ambiguous = true
				break
			}
		}
		if !ambiguous {
			payload = bytes.ReplaceAll(payload, []byte(from), []byte(to))
		}
	}
	return payload
}

// restoreCodexFingerprintResponsePayload 将上游回显的收敛身份恢复为客户端原始身份。
// 映射仅保存在单次账号尝试内，既不会跨账号共享，也不会影响内部调度与缓存键。
func restoreCodexFingerprintResponsePayload(payload []byte, ids *codexFingerprintIDs) []byte {
	if len(payload) == 0 || ids == nil || ids.mode == codexFingerprintOff {
		return payload
	}
	if restored, changed := restoreCodexFingerprintJSONPayload(payload, ids); changed {
		return restored
	}
	if restored, changed := restoreCodexFingerprintSSEPayload(payload, ids); changed {
		return restored
	}
	return restoreUnambiguousCodexFingerprintPayload(payload, ids)
}

// restoreStagedCodexFingerprintResponsePayload 使用当前 Gin 请求暂存的账号级映射。
func restoreStagedCodexFingerprintResponsePayload(c *gin.Context, payload []byte) []byte {
	return restoreCodexFingerprintResponsePayload(payload, stagedCodexFingerprintIDs(c))
}

// applyCodexFingerprintHeaders 按预计算的收敛 ID 改写出站 HTTP 头中的设备指纹。
// 在 buildUpstreamRequest 完成白名单透传和身份头收口后调用。
func applyCodexFingerprintHeaders(h http.Header, ids *codexFingerprintIDs) {
	if h == nil || ids == nil {
		return
	}
	if !ids.extendedTurnIdentity {
		stripUnsupportedCodexExtendedTurnIdentity(h)
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
	if ids.extendedTurnIdentity && ids.originalParentThreadID != "" {
		if ids.parentThreadID != "" {
			h.Set("x-codex-parent-thread-id", ids.parentThreadID)
		} else {
			h.Del("x-codex-parent-thread-id")
		}
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && strings.TrimSpace(h.Get("conversation_id")) != "" {
		h.Set("conversation_id", ids.promptCacheKey)
	}

	fields := map[string]any{
		"installation_id": ids.installationID,
		"session_id":      ids.sessionID,
		"thread_id":       ids.threadID,
		"window_id":       ids.windowID,
	}
	if shouldWriteCodexTurnID(ids) {
		fields["turn_id"] = ids.turnID
	}
	if shouldWriteCodexTurnStartedAt(ids) {
		fields["turn_started_at_unix_ms"] = ids.turnStartedAtUnixMS
	}
	if ids.extendedTurnIdentity {
		fields["parent_thread_id"] = nil
		fields["x-codex-parent-thread-id"] = nil
		fields["parent_turn_id"] = nil
		fields["root_turn_id"] = nil
		if shouldWriteCodexFirstWindowID(ids) {
			fields["first_window_id"] = ids.firstWindowID
		}
		if shouldManageCodexPreviousWindowID(ids) {
			fields["previous_window_id"] = nil
		}
		if shouldManageCodexPreviousWindowID(ids) && ids.previousWindowID != "" {
			fields["previous_window_id"] = ids.previousWindowID
		}
	}
	if shouldWriteCodexWindowNumber(ids) {
		fields["window_number"] = ids.windowNumber
	}
	if ids.parentTurnID != "" {
		fields["parent_turn_id"] = ids.parentTurnID
	}
	if ids.extendedTurnIdentity && ids.parentThreadID != "" {
		fields["parent_thread_id"] = ids.parentThreadID
	}
	if ids.rootTurnID != "" {
		fields["root_turn_id"] = ids.rootTurnID
	}
	if shouldWriteCodexContextWindowID(ids) {
		fields["context_window_id"] = ids.contextWindowID
	}
	rewriteCodexTurnMetadataFields(h, fields)
}

// rewriteCodexTurnMetadataFields 解析 x-codex-turn-metadata 头中的 JSON，
// 替换指定字段后回写，nil 表示删除。未指定字段（如 sandbox）保持原样。
func rewriteCodexTurnMetadataFields(h http.Header, fields map[string]any) {
	raw := strings.TrimSpace(h.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		return
	}
	for k, v := range fields {
		if v == nil {
			delete(metadata, k)
		} else {
			metadata[k] = v
		}
	}
	rebuilt, err := json.Marshal(metadata)
	if err != nil {
		return
	}
	h.Set("x-codex-turn-metadata", string(rebuilt))
}

// stripUnsupportedCodexExtendedTurnIdentity removes fields introduced after
// Codex 0.151 from requests sent on older client wire contracts.
func stripUnsupportedCodexExtendedTurnIdentity(h http.Header) {
	if h == nil {
		return
	}
	for _, key := range []string{
		"parent_thread_id", "parent_turn_id", "root_turn_id", "context_window_id", "window_number", "first_window_id", "previous_window_id",
		"x-codex-parent-thread-id",
		"x-codex-parent-turn-id", "x-codex-root-turn-id", "x-codex-context-window-id",
	} {
		h.Del(key)
	}
	raw := strings.TrimSpace(h.Get("x-codex-turn-metadata"))
	if raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return
	}
	if stripUnsupportedCodexExtendedTurnIdentityMap(metadata) {
		if rebuilt, err := json.Marshal(metadata); err == nil {
			h.Set("x-codex-turn-metadata", string(rebuilt))
		}
	}
}

func stripUnsupportedCodexExtendedTurnIdentityMap(values map[string]any) bool {
	if values == nil {
		return false
	}
	modified := false
	for _, key := range []string{
		"parent_thread_id", "parent_turn_id", "root_turn_id", "context_window_id", "window_number", "first_window_id", "previous_window_id",
		"parent-thread-id", "x-codex-parent-thread-id",
		"parent-turn-id", "root-turn-id", "context-window-id", "window-number", "first-window-id", "previous-window-id",
		"x-codex-parent-turn-id", "x-codex-root-turn-id", "x-codex-context-window-id",
	} {
		if _, exists := values[key]; exists {
			delete(values, key)
			modified = true
		}
	}
	if raw, ok := values["x-codex-turn-metadata"].(string); ok && strings.TrimSpace(raw) != "" {
		var nested map[string]any
		if err := json.Unmarshal([]byte(raw), &nested); err == nil && stripUnsupportedCodexExtendedTurnIdentityMap(nested) {
			if rebuilt, err := json.Marshal(nested); err == nil {
				values["x-codex-turn-metadata"] = string(rebuilt)
				modified = true
			}
		}
	}
	return modified
}

func stripUnsupportedCodexExtendedTurnIdentityBody(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	modified := stripUnsupportedCodexExtendedTurnIdentityMap(reqBody)
	if metadata, ok := reqBody["client_metadata"].(map[string]any); ok && stripUnsupportedCodexExtendedTurnIdentityMap(metadata) {
		modified = true
	}
	return modified
}

// codexMetadataOnlyBodyFields 只允许作为元数据传递，不属于 Responses 顶层参数。
var codexMetadataOnlyBodyFields = []string{
	"parent_thread_id", "root_turn_id", "parent_turn_id", "context_window_id", "window_number", "first_window_id", "previous_window_id",
}

// preserveCodexTopLevelMetadata 在删除兼容客户端的顶层字段前保留有效值。
// device 模式也只迁移载体，不改动客户端的回合与窗口身份。
func preserveCodexTopLevelMetadata(body, metadata map[string]any) {
	for _, key := range codexMetadataOnlyBodyFields {
		if key == "window_number" {
			if number, ok := extractCodexWindowNumberField(body, key); ok {
				metadata[key] = strconv.FormatUint(number, 10)
			}
		} else if value := extractCodexStringField(body, key); value != "" {
			metadata[key] = value
		}
	}
}

// applyCodexFingerprintClientMetadata 按预计算的收敛 ID 改写请求体中的 client_metadata。
// 使用与头改写相同的 ids 实例，确保 turn_id 等随机字段一致。
func applyCodexFingerprintClientMetadata(reqBody map[string]any, ids *codexFingerprintIDs) bool {
	if reqBody == nil || ids == nil {
		return false
	}
	if !ids.extendedTurnIdentity {
		stripUnsupportedCodexExtendedTurnIdentityBody(reqBody)
	}

	existing, _ := reqBody["client_metadata"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}
	preserveCodexTopLevelMetadata(reqBody, existing)
	if !applyCodexFingerprintToClientMetadataMap(existing, ids) {
		return false
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && ids.promptCacheKeyInBody {
		reqBody["prompt_cache_key"] = ids.promptCacheKey
	}
	// 官方生成协议不接受这些顶层字段；已有有效值已迁入元数据。
	for _, key := range codexMetadataOnlyBodyFields {
		delete(reqBody, key)
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
	if !ids.extendedTurnIdentity {
		stripUnsupportedCodexExtendedTurnIdentityMap(existing)
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
	if shouldWriteCodexTurnID(ids) {
		existing["turn_id"] = ids.turnID
	} else if ids.mode == codexFingerprintCockpit {
		delete(existing, "turn_id")
	}
	if shouldWriteCodexTurnStartedAt(ids) {
		// 平铺开始时间也是已支持的输入载体。只对已有有效值同步生命周期时间，
		// 保留字符串/数字类别；缺失或异常字段不补造、不改变原有类型处理。
		if _, present := extractCodexTurnStartedAtField(existing, "turn_started_at_unix_ms"); present {
			if _, isString := existing["turn_started_at_unix_ms"].(string); isString {
				existing["turn_started_at_unix_ms"] = strconv.FormatInt(ids.turnStartedAtUnixMS, 10)
			} else {
				existing["turn_started_at_unix_ms"] = ids.turnStartedAtUnixMS
			}
		}
	}
	existing["x-codex-window-id"] = ids.windowID
	if ids.extendedTurnIdentity {
		delete(existing, "parent_thread_id")
		delete(existing, "x-codex-parent-thread-id")
		delete(existing, "parent_turn_id")
		delete(existing, "root_turn_id")
		if shouldWriteCodexFirstWindowID(ids) {
			existing["first_window_id"] = ids.firstWindowID
		}
		if shouldManageCodexPreviousWindowID(ids) {
			delete(existing, "previous_window_id")
		}
		if shouldManageCodexPreviousWindowID(ids) && ids.previousWindowID != "" {
			existing["previous_window_id"] = ids.previousWindowID
		}
	}
	if shouldWriteCodexWindowNumber(ids) {
		existing["window_number"] = strconv.FormatUint(ids.windowNumber, 10)
	} else if ids.mode == codexFingerprintCockpit {
		delete(existing, "window_number")
	}
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		existing["parent_turn_id"] = ids.parentTurnID
	}
	if ids.extendedTurnIdentity && ids.parentThreadID != "" {
		existing["parent_thread_id"] = ids.parentThreadID
	}
	if ids.rootTurnID != "" {
		existing["root_turn_id"] = ids.rootTurnID
	}
	if shouldWriteCodexContextWindowID(ids) {
		existing["context_window_id"] = ids.contextWindowID
	}

	fields := map[string]any{
		"installation_id": ids.installationID,
		"session_id":      ids.sessionID,
		"thread_id":       ids.threadID,
		"window_id":       ids.windowID,
	}
	if shouldWriteCodexTurnID(ids) {
		fields["turn_id"] = ids.turnID
	}
	if shouldWriteCodexTurnStartedAt(ids) {
		fields["turn_started_at_unix_ms"] = ids.turnStartedAtUnixMS
	}
	if ids.extendedTurnIdentity {
		fields["parent_thread_id"] = nil
		fields["x-codex-parent-thread-id"] = nil
		fields["parent_turn_id"] = nil
		fields["root_turn_id"] = nil
		if shouldWriteCodexFirstWindowID(ids) {
			fields["first_window_id"] = ids.firstWindowID
		}
		if shouldManageCodexPreviousWindowID(ids) {
			fields["previous_window_id"] = nil
		}
		if shouldManageCodexPreviousWindowID(ids) && ids.previousWindowID != "" {
			fields["previous_window_id"] = ids.previousWindowID
		}
	}
	if shouldWriteCodexWindowNumber(ids) {
		fields["window_number"] = ids.windowNumber
	}
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		fields["parent_turn_id"] = ids.parentTurnID
	}
	if ids.extendedTurnIdentity && ids.parentThreadID != "" {
		fields["parent_thread_id"] = ids.parentThreadID
	}
	if ids.rootTurnID != "" {
		fields["root_turn_id"] = ids.rootTurnID
	}
	if shouldWriteCodexContextWindowID(ids) {
		fields["context_window_id"] = ids.contextWindowID
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
	// 仅抽取小范围身份字段，不解码 input/tools 等大对象。
	topLevel := make(map[string]any)
	for _, key := range codexMetadataOnlyBodyFields {
		value := gjson.GetBytes(body, key)
		if value.Type == gjson.String {
			topLevel[key] = value.Str
		} else if key == "window_number" && value.Type == gjson.Number {
			topLevel[key] = json.Number(value.Raw)
		}
	}
	preserveCodexTopLevelMetadata(topLevel, existing)
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
	// 与解码路径保持一致，扩展身份字段只保留在元数据中。
	for _, path := range codexMetadataOnlyBodyFields {
		updated, err = sjson.DeleteBytes(updated, path)
		if err != nil {
			return body, false, fmt.Errorf("remove unsupported top-level %s: %w", path, err)
		}
	}
	return updated, true, nil
}

// rewriteClientMetadataEmbeddedTurnMetadata 改写 client_metadata 中内嵌的
// x-codex-turn-metadata JSON 字符串里的指定字段，nil 表示删除。
func rewriteClientMetadataEmbeddedTurnMetadata(clientMetadata map[string]any, fields map[string]any) {
	raw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	if !ok || raw == "" {
		return
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		return
	}
	for k, v := range fields {
		if v == nil {
			delete(metadata, k)
		} else {
			metadata[k] = v
		}
	}
	if rebuilt, err := json.Marshal(metadata); err == nil {
		clientMetadata["x-codex-turn-metadata"] = string(rebuilt)
	}
}
