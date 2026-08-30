package service

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
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
	codexIdentityBindingIdleTTL    = 7 * 24 * time.Hour
	codexIdentityBindingTouchEvery = 5 * time.Minute
	codexIdentityHotCacheTTL       = 24 * time.Hour
	codexIdentityBindingMaxEntries = 1024
)

// CodexIdentityBindingsExtraKey stores complete UUIDv7 values keyed by a
// hashed conversation seed.  The binding is persisted with the OAuth account
// so a process restart or a second gateway instance reuses the same UUID
// instead of creating a new timestamped identity and breaking cache affinity.
const CodexIdentityBindingsExtraKey = "codex_identity_bindings_v1"

type codexIdentityBinding struct {
	UUID         string `json:"uuid"`
	CreatedAtMS  int64  `json:"created_at_ms"`
	LastUsedAtMS int64  `json:"last_used_at_ms"`
}

var codexIdentityBindingLocks sync.Map    // account ID -> *sync.Mutex
var codexIdentityPersistedHashes sync.Map // account ID -> sha256 of bindings JSON
var codexIdentityHotCache sync.Map        // account+seed -> codexIdentityHotBinding
var codexIdentityHotCacheOps atomic.Uint64

// codexPromptCacheCarry 保存同一会话最近一次明确的客户端缓存键。
// 仅用于普通 Responses 请求的一轮缺失容错，避免压缩边界造成命名空间跳变。
var codexPromptCacheCarry = struct {
	sync.Mutex
	items map[string]codexPromptCacheCarryEntry
}{items: make(map[string]codexPromptCacheCarryEntry)}

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

func rememberCodexPromptCacheKey(account *Account, ids *codexFingerprintIDs, key string, inBody bool) {
	if account == nil || ids == nil || strings.TrimSpace(key) == "" {
		return
	}
	now := time.Now().UnixMilli()
	cacheKey := codexPromptCacheCarryKey(account.ID, ids.sessionID, ids.threadID, ids.windowID)
	codexPromptCacheCarry.Lock()
	defer codexPromptCacheCarry.Unlock()
	if _, exists := codexPromptCacheCarry.items[cacheKey]; !exists && len(codexPromptCacheCarry.items) >= codexPromptCacheCarryMaxEntries {
		var oldestKey string
		oldest := now
		for candidate, entry := range codexPromptCacheCarry.items {
			if entry.LastUsedAt <= oldest {
				oldestKey, oldest = candidate, entry.LastUsedAt
			}
		}
		if oldestKey != "" {
			delete(codexPromptCacheCarry.items, oldestKey)
		}
	}
	codexPromptCacheCarry.items[cacheKey] = codexPromptCacheCarryEntry{Key: strings.TrimSpace(key), WindowID: ids.windowID, InBody: inBody, LastUsedAt: now}
}

func loadCodexPromptCacheKey(account *Account, ids *codexFingerprintIDs) (codexPromptCacheCarryEntry, bool) {
	if account == nil || ids == nil {
		return codexPromptCacheCarryEntry{}, false
	}
	cacheKey := codexPromptCacheCarryKey(account.ID, ids.sessionID, ids.threadID, ids.windowID)
	now := time.Now().UnixMilli()
	codexPromptCacheCarry.Lock()
	defer codexPromptCacheCarry.Unlock()
	entry, ok := codexPromptCacheCarry.items[cacheKey]
	if !ok || now-entry.LastUsedAt > codexIdentityBindingIdleTTL.Milliseconds() || entry.WindowID != ids.windowID {
		if ok {
			delete(codexPromptCacheCarry.items, cacheKey)
		}
		return codexPromptCacheCarryEntry{}, false
	}
	entry.LastUsedAt = now
	codexPromptCacheCarry.items[cacheKey] = entry
	return entry, true
}

func codexIdentityBindingLock(accountID int64) *sync.Mutex {
	if existing, ok := codexIdentityBindingLocks.Load(accountID); ok {
		if mutex, ok := existing.(*sync.Mutex); ok {
			return mutex
		}
	}
	created := &sync.Mutex{}
	actual, _ := codexIdentityBindingLocks.LoadOrStore(accountID, created)
	if mutex, ok := actual.(*sync.Mutex); ok {
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

func readCodexIdentityBindings(account *Account) map[string]any {
	if account == nil || account.Extra == nil {
		return nil
	}
	raw, ok := account.Extra[CodexIdentityBindingsExtraKey]
	if !ok {
		return nil
	}
	if bindings, ok := raw.(map[string]any); ok {
		return bindings
	}
	return nil
}

func parseCodexIdentityBinding(raw any) (codexIdentityBinding, bool) {
	switch value := raw.(type) {
	case string:
		if parsed, err := uuid.Parse(value); err == nil && parsed.Version() == uuid.Version(7) && parsed.Variant() == uuid.RFC4122 {
			return codexIdentityBinding{UUID: parsed.String()}, true
		}
	case map[string]any:
		candidate, _ := value["uuid"].(string)
		parsed, err := uuid.Parse(strings.TrimSpace(candidate))
		if err != nil || parsed.Version() != uuid.Version(7) || parsed.Variant() != uuid.RFC4122 {
			return codexIdentityBinding{}, false
		}
		created, _ := value["created_at_ms"].(float64)
		lastUsed, _ := value["last_used_at_ms"].(float64)
		return codexIdentityBinding{UUID: parsed.String(), CreatedAtMS: int64(created), LastUsedAtMS: int64(lastUsed)}, true
	case codexIdentityBinding:
		parsed, err := uuid.Parse(value.UUID)
		if err == nil && parsed.Version() == uuid.Version(7) && parsed.Variant() == uuid.RFC4122 {
			return value, true
		}
	}
	return codexIdentityBinding{}, false
}

// deriveStableUUIDv7ForAccount first consults the account's durable binding.
// On a miss it generates exactly once using the current Unix millisecond and
// records the complete UUID in Extra.  Callers persist the changed Extra via
// AccountRepository after the request snapshot is built.
func deriveStableUUIDv7ForAccount(account *Account, seed string) string {
	seed = strings.TrimSpace(seed)
	if account == nil || seed == "" {
		return deriveStableUUIDv7(seed)
	}
	lock := codexIdentityBindingLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	key := codexIdentitySeedKey(seed)
	bindings := readCodexIdentityBindings(account)
	nowMS := time.Now().UnixMilli()
	if bindings != nil {
		pruneCodexIdentityBindings(bindings, nowMS, key)
		if binding, ok := parseCodexIdentityBinding(bindings[key]); ok {
			if binding.CreatedAtMS == 0 {
				binding.CreatedAtMS = nowMS
			}
			if binding.LastUsedAtMS == 0 || nowMS-binding.LastUsedAtMS >= codexIdentityBindingTouchEvery.Milliseconds() {
				binding.LastUsedAtMS = nowMS
				bindings[key] = binding
			}
			codexIdentityHotCache.Store(codexIdentityHotKey(account, seed), codexIdentityHotBinding{UUID: binding.UUID, LastUsedAtMS: nowMS})
			sweepCodexIdentityHotCache()
			return binding.UUID
		}
	}
	hotKey := codexIdentityHotKey(account, seed)
	if raw, ok := codexIdentityHotCache.Load(hotKey); ok {
		if hot, valid := raw.(codexIdentityHotBinding); valid && hot.UUID != "" && (hot.LastUsedAtMS == 0 || nowMS-hot.LastUsedAtMS < codexIdentityHotCacheTTL.Milliseconds()) {
			if bindings == nil {
				bindings = make(map[string]any)
				if account.Extra == nil {
					account.Extra = make(map[string]any)
				}
				account.Extra[CodexIdentityBindingsExtraKey] = bindings
			}
			bindings[key] = codexIdentityBinding{UUID: hot.UUID, CreatedAtMS: nowMS, LastUsedAtMS: nowMS}
			codexIdentityHotCache.Store(hotKey, codexIdentityHotBinding{UUID: hot.UUID, LastUsedAtMS: nowMS})
			return hot.UUID
		}
		codexIdentityHotCache.Delete(hotKey)
	}
	value := newCodexUUIDv7().String()
	if bindings == nil {
		bindings = make(map[string]any)
		if account.Extra == nil {
			account.Extra = make(map[string]any)
		}
		account.Extra[CodexIdentityBindingsExtraKey] = bindings
	}
	bindings[key] = codexIdentityBinding{UUID: value, CreatedAtMS: nowMS, LastUsedAtMS: nowMS}
	codexIdentityHotCache.Store(hotKey, codexIdentityHotBinding{UUID: value, LastUsedAtMS: nowMS})
	pruneCodexIdentityBindings(bindings, nowMS, key)
	sweepCodexIdentityHotCache()
	return value
}

// pruneCodexIdentityBindings applies a sliding idle TTL and true LRU cap to
// durable server-derived identities.  The durable account map is authoritative;
// entries removed here are also removed from the process-local hot cache so an
// evicted identity cannot be resurrected after the next request.
func pruneCodexIdentityBindings(bindings map[string]any, nowMS int64, protectedKey string) bool {
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
		lastUsed := binding.LastUsedAtMS
		if lastUsed == 0 {
			lastUsed = binding.CreatedAtMS
		}
		if lastUsed > 0 && nowMS-lastUsed >= codexIdentityBindingIdleTTL.Milliseconds() {
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
	}
	for len(bindings) > codexIdentityBindingMaxEntries {
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

// persistCodexIdentityBindings writes only the bounded identity map.  The
// hash gate keeps the hot path read-only after the first binding, while the
// account-scoped lock prevents concurrent first requests from overwriting one
// another in the local object.  Repository failures are returned to callers so
// deployments can log them without changing the request identity snapshot.
func persistCodexIdentityBindings(ctx context.Context, repo AccountRepository, account *Account) (err error) {
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
	lock := codexIdentityBindingLock(account.ID)
	lock.Lock()
	defer lock.Unlock()
	if account.Extra == nil {
		return nil
	}
	if _, exists := account.Extra[CodexIdentityBindingsExtraKey]; !exists {
		return nil
	}
	bindings := readCodexIdentityBindings(account)
	if bindings == nil {
		bindings = make(map[string]any)
		account.Extra[CodexIdentityBindingsExtraKey] = bindings
	}
	pruneCodexIdentityBindings(bindings, time.Now().UnixMilli(), "")

	// Merge with the latest row before writing.  Two requests can arrive on
	// distinct Account objects during startup; without this read-modify-merge,
	// the second first-seen conversation could erase the first binding.
	if latest, err := repo.GetByID(ctx, account.ID); err == nil && latest != nil {
		latestBindings := readCodexIdentityBindings(latest)
		if latestBindings != nil {
			pruneCodexIdentityBindings(latestBindings, time.Now().UnixMilli(), "")
			bindings = mergeCodexIdentityBindings(latestBindings, bindings)
			pruneCodexIdentityBindings(bindings, time.Now().UnixMilli(), "")
			account.Extra[CodexIdentityBindingsExtraKey] = bindings
		}
	}
	// An expired/invalid map is intentionally persisted as an empty object so
	// stale rows are removed from Extra instead of surviving indefinitely.
	encoded, err := json.Marshal(bindings)
	if err != nil {
		return fmt.Errorf("marshal codex identity bindings: %w", err)
	}
	hash := sha256.Sum256(encoded)
	hashKey := fmt.Sprintf("%d", account.ID)
	hashString := fmt.Sprintf("%x", hash[:])
	if previous, ok := codexIdentityPersistedHashes.Load(hashKey); ok && previous == hashString {
		return nil
	}
	if err := repo.UpdateExtra(ctx, account.ID, map[string]any{CodexIdentityBindingsExtraKey: bindings}); err != nil {
		return fmt.Errorf("persist codex identity bindings: %w", err)
	}
	codexIdentityPersistedHashes.Store(hashKey, hashString)
	return nil
}

// codexUUIDv7Context mirrors uuid 1.20.0's shared ContextV7 used by
// Codex's Rust runtime: a 41-bit random seed is selected whenever the
// millisecond changes, then a 42-bit counter advances monotonically while
// the clock is stationary or moves backwards.
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

// encodeCodexUUIDv7 applies uuid 1.20.0's counter/variant layout exactly.
// The top 12 counter bits are shifted around the RFC variant gap; the
// remaining 30 counter bits follow it, and the rest of the 74-bit suffix is
// random.
func encodeCodexUUIDv7(timestampMS, counter uint64, randomBytes [16]byte) uuid.UUID {
	counter &= codexUUIDv7MaxCounter
	counter44 := (counter & ((uint64(1) << 30) - 1)) | ((counter >> 30) << 32)

	var cr [16]byte
	cb := [6]byte{
		byte(counter44 >> 36),
		byte(counter44 >> 28),
		byte(counter44 >> 20),
		byte(counter44 >> 12),
		byte(counter44 >> 4),
		byte(counter44 & 0x0f),
	}
	cr[0], cr[1], cr[2], cr[3], cr[4] = cb[0], cb[1], cb[2], cb[3], cb[4]
	cr[5] = (cb[5] << 4) | (randomBytes[5] & 0x0f)
	copy(cr[6:], randomBytes[6:])

	var out [16]byte
	out[0] = byte(timestampMS >> 40)
	out[1] = byte(timestampMS >> 32)
	out[2] = byte(timestampMS >> 24)
	out[3] = byte(timestampMS >> 16)
	out[4] = byte(timestampMS >> 8)
	out[5] = byte(timestampMS)
	out[6] = 0x70 | (cr[0] & 0x0f)
	out[7] = cr[1]
	out[8] = 0x80 | (cr[2] & 0x3f)
	copy(out[9:], cr[3:10])
	return uuid.UUID(out)
}

func newCodexUUIDv7() uuid.UUID {
	now := time.Now().UnixMilli()
	if now < 0 {
		now = 0
	}
	nowMS := uint64(now)

	codexUUIDv7Shared.mu.Lock()
	if !codexUUIDv7Shared.initialized || nowMS > codexUUIDv7Shared.lastSeedMS {
		codexUUIDv7Shared.initialized = true
		codexUUIDv7Shared.timestampMS = nowMS
		codexUUIDv7Shared.lastSeedMS = nowMS
		codexUUIDv7Shared.counter = codexUUIDv7Random41()
	} else {
		codexUUIDv7Shared.counter++
		if codexUUIDv7Shared.counter > codexUUIDv7MaxCounter {
			codexUUIDv7Shared.timestampMS++
			codexUUIDv7Shared.lastSeedMS = codexUUIDv7Shared.timestampMS
			codexUUIDv7Shared.counter = codexUUIDv7Random41()
		}
	}
	timestampMS := codexUUIDv7Shared.timestampMS
	counter := codexUUIDv7Shared.counter
	codexUUIDv7Shared.mu.Unlock()

	var randomBytes [16]byte
	if _, err := cryptorand.Read(randomBytes[:]); err != nil {
		return uuid.Must(uuid.NewV7())
	}
	return encodeCodexUUIDv7(timestampMS, counter, randomBytes)
}

// deriveStableUUIDv7 为缺少官方身份字段的桥接请求生成一次 UUIDv7。
// 生成器使用与 Codex Rust uuid 1.20.0 相同的 ContextV7 低 74 位布局；
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

// resolveConvergedContextWindowID mirrors Codex 0.151's context-window lifecycle:
// one independently generated UUIDv7 belongs to each final thread/window
// generation, remains stable while that window is active, and changes only when
// the generation advances. The durable account binding keeps resume/restart
// behavior stable without making the client-supplied context ID authoritative.
func resolveConvergedContextWindowID(account *Account, threadID, windowID string) string {
	if account == nil || strings.TrimSpace(threadID) == "" {
		return ""
	}
	generation := "0"
	if idx := strings.LastIndex(strings.TrimSpace(windowID), ":"); idx >= 0 {
		if value := strings.TrimSpace(windowID[idx+1:]); value != "" {
			if _, err := strconv.ParseUint(value, 10, 64); err == nil {
				generation = value
			}
		}
	}
	seed := fmt.Sprintf("codex-context-window:%s:%s", strings.TrimSpace(threadID), generation)
	return deriveStableUUIDv7ForAccount(account, seed)
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
	if account == nil || strings.TrimSpace(conversationSeed) == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, fmt.Sprintf("sub2api:codex-cockpit-session-id:v2:%s:%s", seed, strings.TrimSpace(conversationSeed)))
}

// resolveConvergedThreadID 按客户端原始 session-id 确定性派生 thread_id。
// 每个真实 Codex 会话（不同客户端启动实例）获得一个独立线程，
// 模拟正常用户 spawn 子代理或开多窗口的模式。
func resolveConvergedThreadID(account *Account, clientSessionID string) string {
	if account == nil || clientSessionID == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, fmt.Sprintf("sub2api:codex-thread-id:v3:%s:%s", seed, clientSessionID))
}

// resolveCodexRootTurnID follows Codex turn-tree semantics without creating an
// account-persistent root mapping. A supplied root without a parent denotes a
// top-level turn, whose root is the newly generated turn ID. A supplied root
// with a parent is a child lineage value and is inherited verbatim. Missing
// root values remain missing for older clients such as Codex 0.145.
func resolveCodexRootTurnID(originalRootTurnID, parentTurnID, turnID string) string {
	originalRootTurnID = strings.TrimSpace(originalRootTurnID)
	if originalRootTurnID == "" {
		return ""
	}
	if strings.TrimSpace(parentTurnID) == "" {
		return strings.TrimSpace(turnID)
	}
	return originalRootTurnID
}

// resolveConvergedPromptCacheKey 按账号和客户端原始缓存键稳定派生上游缓存键。
// 相同账号的相同对话保持稳定，不同账号或不同对话互相隔离。
func resolveConvergedPromptCacheKey(account *Account, promptCacheKey string) string {
	if account == nil || strings.TrimSpace(promptCacheKey) == "" {
		return ""
	}
	seed := resolveCodexFingerprintSeed(account)
	if seed == "" {
		return ""
	}
	return deriveStableUUIDv7ForAccount(account, fmt.Sprintf("sub2api:codex-prompt-cache-key:v3:%s:%s", seed, strings.TrimSpace(promptCacheKey)))
}

// resolveOfficialCockpitPromptCacheKey 对齐 Codex 默认规则：显式缓存键原样保留，
// 缺省时使用根 session_id；不再为缓存键额外构造一个带伪时间戳的 UUIDv7。
func resolveOfficialCockpitPromptCacheKey(sessionID, promptCacheKey string) string {
	if key := strings.TrimSpace(promptCacheKey); key != "" {
		return key
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

// codexFingerprintSource 保存客户端原始身份字段，供不同模式选择派生种子。
type codexFingerprintSource struct {
	clientVersion         string
	installationID        string
	clientSessionID       string
	originalSessionID     string
	threadID              string
	turnID                string
	parentTurnID          string
	rootTurnID            string
	windowID              string
	contextWindowID       string
	promptCacheKey        string
	promptCacheKeyInBody  bool
	allowPromptCacheCarry bool
	rootTurnIDInBody      bool
	contextWindowIDInBody bool
}

// codexFingerprintIDs 收敛后的完整 ID 集合。
// 由 resolveCodexFingerprintIDs 一次性生成，同一个实例在头改写和体改写之间共享，
// 确保所有载体中的 turn_id 等随机字段一致。
type codexFingerprintIDs struct {
	// stagedAccountID 记录本次请求实际调度的账号。Spark 影子的身份字段由父账号
	// 派生，但暂存值只能由同一个影子尝试读取，避免 OAuth→OAuth failover 误用。
	stagedAccountID         int64
	stagedAccountBound      bool
	mode                    codexFingerprintMode
	extendedTurnIdentity    bool
	originalInstallationID  string
	installationID          string
	originalSessionID       string
	sessionID               string
	originalThreadID        string
	threadID                string
	originalTurnID          string
	turnID                  string
	originalParentTurnID    string
	parentTurnID            string
	originalRootTurnID      string
	rootTurnID              string
	originalContextWindowID string
	contextWindowID         string
	contextWindowIDInBody   bool
	turnStartedAtUnixMS     int64
	originalWindowID        string
	windowID                string
	originalPromptCacheKey  string
	promptCacheKey          string
	// promptCacheKeyInBody 区分原请求体字段与仅用于 Header 的兼容缓存键。
	promptCacheKeyInBody bool
	rootTurnIDInBody     bool
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
// 注意：包含随机生成的 turn_id，调用方必须只调用一次并共享结果给头改写和体改写。
func resolveCodexFingerprintIDs(account *Account, clientSessionID string, mode codexFingerprintMode) *codexFingerprintIDs {
	return resolveCodexFingerprintIDsWithSource(account, codexFingerprintSource{clientSessionID: clientSessionID}, mode)
}

// resolveCodexFingerprintIDsWithSource 使用完整客户端身份来源计算出站 ID 集合。
func resolveCodexFingerprintIDsWithSource(account *Account, source codexFingerprintSource, mode codexFingerprintMode) *codexFingerprintIDs {
	if mode == codexFingerprintOff {
		return nil
	}

	ids := &codexFingerprintIDs{
		mode:                    mode,
		extendedTurnIdentity:    codexSupportsExtendedTurnIdentity(source.clientVersion),
		turnStartedAtUnixMS:     time.Now().UnixMilli(),
		originalInstallationID:  strings.TrimSpace(source.installationID),
		originalSessionID:       strings.TrimSpace(source.originalSessionID),
		originalThreadID:        strings.TrimSpace(source.threadID),
		originalTurnID:          strings.TrimSpace(source.turnID),
		originalParentTurnID:    strings.TrimSpace(source.parentTurnID),
		originalRootTurnID:      strings.TrimSpace(source.rootTurnID),
		originalContextWindowID: strings.TrimSpace(source.contextWindowID),
		originalWindowID:        strings.TrimSpace(source.windowID),
		originalPromptCacheKey:  strings.TrimSpace(source.promptCacheKey),
		rootTurnIDInBody:        source.rootTurnIDInBody,
		contextWindowIDInBody:   source.contextWindowIDInBody,
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
		ids.turnID = newCodexUUIDv7().String()
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = resolveCodexRootTurnID(ids.originalRootTurnID, ids.originalParentTurnID, ids.turnID)
		}
		ids.windowID = normalizeCodexWindowID(source.windowID, ids.threadID)
		if ids.extendedTurnIdentity {
			ids.contextWindowID = resolveConvergedContextWindowID(account, ids.threadID, ids.windowID)
		}
		return ids

	case codexFingerprintCockpit:
		// Cockpit keeps device and conversation identity server-derived. Client
		// conversation fields are used only as a stable seed; the explicit
		// prompt_cache_key remains the one client-controlled cache identity.
		sessionSeed := resolveCockpitSessionSeed(source)
		threadSeed := resolveCockpitThreadSeed(source)
		ids.sessionID = resolveConvergedCockpitSessionID(account, sessionSeed)
		if ids.sessionID == "" {
			ids.sessionID = resolveConvergedSessionID(account)
		}
		if ids.sessionID == "" {
			return nil
		}

		ids.threadID = resolveConvergedThreadID(account, threadSeed)
		if ids.threadID == "" {
			ids.threadID = ids.sessionID
		}

		ids.turnID = newCodexUUIDv7().String()
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = resolveCodexRootTurnID(ids.originalRootTurnID, ids.originalParentTurnID, ids.turnID)
		}
		ids.windowID = normalizeCodexWindowID(source.windowID, ids.threadID)
		if ids.extendedTurnIdentity {
			ids.contextWindowID = resolveConvergedContextWindowID(account, ids.threadID, ids.windowID)
		}
		if strings.TrimSpace(source.promptCacheKey) != "" {
			ids.promptCacheKey = strings.TrimSpace(source.promptCacheKey)
			rememberCodexPromptCacheKey(account, ids, ids.promptCacheKey, source.promptCacheKeyInBody)
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
		// 显式键保持原载体；短暂复用时沿用上一次键的载体形态。
		if strings.TrimSpace(source.promptCacheKey) != "" || !source.allowPromptCacheCarry {
			ids.promptCacheKeyInBody = source.promptCacheKeyInBody
		}
		return ids

	case codexFingerprintFull:
		ids.sessionID = resolveConvergedSessionID(account)
		if ids.sessionID == "" {
			return nil
		}
		ids.threadID = ids.sessionID
		ids.turnID = newCodexUUIDv7().String()
		if ids.extendedTurnIdentity {
			ids.parentTurnID = ids.originalParentTurnID
			ids.rootTurnID = resolveCodexRootTurnID(ids.originalRootTurnID, ids.originalParentTurnID, ids.turnID)
		}
		ids.windowID = normalizeCodexWindowID(source.windowID, ids.threadID)
		if ids.extendedTurnIdentity {
			ids.contextWindowID = resolveConvergedContextWindowID(account, ids.threadID, ids.windowID)
		}
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

// extractCodexTurnMetadataField 从 JSON 字符串形式的回合元数据读取身份字段。
func extractCodexTurnMetadataField(raw, key string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(raw, key).String())
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
		source.threadID = firstNonEmptyCodexValue(
			extractCodexStringField(reqBody, "thread_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "thread_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "thread_id"),
			h.Get("thread-id"),
		)
	}
	source.turnID = firstNonEmptyCodexValue(
		extractCodexStringField(clientMetadata, "turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "turn_id"),
	)
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
	source.rootTurnIDInBody = extractCodexStringField(reqBody, "root_turn_id") != ""
	source.windowID = firstNonEmptyCodexValue(
		extractCodexStringField(clientMetadata, "x-codex-window-id"),
		extractCodexStringField(reqBody, "window_id"),
		h.Get("x-codex-window-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "window_id"),
	)
	source.contextWindowID = firstNonEmptyCodexValue(
		extractCodexStringField(reqBody, "context_window_id"),
		extractCodexStringField(clientMetadata, "context_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "context_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "context_window_id"),
		h.Get("x-codex-context-window-id"),
	)
	source.contextWindowIDInBody = extractCodexStringField(reqBody, "context_window_id") != ""
	source.promptCacheKey = extractCodexStringField(reqBody, "prompt_cache_key")
	source.promptCacheKeyInBody = source.promptCacheKey != ""
	if source.promptCacheKey == "" {
		source.promptCacheKey = firstNonEmptyCodexValue(
			h.Get("conversation_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "prompt_cache_key"),
			extractCodexTurnMetadataField(headerTurnMetadata, "prompt_cache_key"),
		)
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
		return strings.TrimSpace(gjson.GetBytes(body, path).String())
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
		source.threadID = firstNonEmptyCodexValue(
			read("thread_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "thread_id"),
			extractCodexTurnMetadataField(headerTurnMetadata, "thread_id"),
			h.Get("thread-id"),
		)
	}
	source.turnID = firstNonEmptyCodexValue(
		read("client_metadata.turn_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "turn_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "turn_id"),
	)
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
	source.rootTurnIDInBody = read("root_turn_id") != ""
	source.windowID = firstNonEmptyCodexValue(
		read("client_metadata.x-codex-window-id"),
		read("window_id"),
		h.Get("x-codex-window-id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "window_id"),
	)
	source.contextWindowID = firstNonEmptyCodexValue(
		read("context_window_id"),
		read("client_metadata.context_window_id"),
		extractCodexTurnMetadataField(embeddedTurnMetadata, "context_window_id"),
		extractCodexTurnMetadataField(headerTurnMetadata, "context_window_id"),
		h.Get("x-codex-context-window-id"),
	)
	source.contextWindowIDInBody = read("context_window_id") != ""
	source.promptCacheKey = read("prompt_cache_key")
	source.promptCacheKeyInBody = source.promptCacheKey != ""
	if source.promptCacheKey == "" {
		source.promptCacheKey = firstNonEmptyCodexValue(
			h.Get("conversation_id"),
			extractCodexTurnMetadataField(embeddedTurnMetadata, "prompt_cache_key"),
			extractCodexTurnMetadataField(headerTurnMetadata, "prompt_cache_key"),
		)
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
		{ids.contextWindowID, ids.originalContextWindowID},
		{ids.promptCacheKey, ids.originalPromptCacheKey},
		{ids.turnID, ids.originalTurnID},
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
	case "turn_id", "turn-id":
		from, to = ids.turnID, ids.originalTurnID
	case "root_turn_id", "root-turn-id", "x-codex-root-turn-id":
		from, to = ids.rootTurnID, ids.originalRootTurnID
	case "window_id", "window-id", "x-codex-window-id":
		from, to = ids.windowID, ids.originalWindowID
	case "context_window_id", "context-window-id", "x-codex-context-window-id":
		from, to = ids.contextWindowID, ids.originalContextWindowID
	case "prompt_cache_key", "prompt-cache-key", "conversation_id":
		from, to = ids.promptCacheKey, ids.originalPromptCacheKey
	default:
		return "", false
	}
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
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
		from := strings.TrimSpace(pair[0])
		to := strings.TrimSpace(pair[1])
		if from == "" || to == "" || from == to || from != value {
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
		from := strings.TrimSpace(pair[0])
		to := strings.TrimSpace(pair[1])
		if from == "" || to == "" || from == to || !bytes.Contains(payload, []byte(from)) {
			continue
		}
		ambiguous := false
		for otherIndex, other := range mappings {
			if otherIndex == index || strings.TrimSpace(other[0]) != from {
				continue
			}
			otherTarget := strings.TrimSpace(other[1])
			if otherTarget != "" && otherTarget != to {
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
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && strings.TrimSpace(h.Get("conversation_id")) != "" {
		h.Set("conversation_id", ids.promptCacheKey)
	}

	fields := map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	}
	if ids.parentTurnID != "" {
		fields["parent_turn_id"] = ids.parentTurnID
	}
	if ids.rootTurnID != "" {
		fields["root_turn_id"] = ids.rootTurnID
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" {
		fields["prompt_cache_key"] = ids.promptCacheKey
	}
	if ids.contextWindowID != "" {
		fields["context_window_id"] = ids.contextWindowID
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

// stripUnsupportedCodexExtendedTurnIdentity removes fields introduced after
// Codex 0.151 from requests sent on older client wire contracts.
func stripUnsupportedCodexExtendedTurnIdentity(h http.Header) {
	if h == nil {
		return
	}
	for _, key := range []string{
		"parent_turn_id", "root_turn_id", "context_window_id",
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
		"parent_turn_id", "root_turn_id", "context_window_id",
		"parent-turn-id", "root-turn-id", "context-window-id",
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
	if !applyCodexFingerprintToClientMetadataMap(existing, ids) {
		return false
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" && ids.promptCacheKeyInBody {
		reqBody["prompt_cache_key"] = ids.promptCacheKey
	}
	if ids.rootTurnID != "" && ids.rootTurnIDInBody {
		reqBody["root_turn_id"] = ids.rootTurnID
	}
	if ids.contextWindowID != "" && ids.contextWindowIDInBody {
		reqBody["context_window_id"] = ids.contextWindowID
	}
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		reqBody["parent_turn_id"] = ids.parentTurnID
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
	existing["turn_id"] = ids.turnID
	existing["x-codex-window-id"] = ids.windowID
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		existing["parent_turn_id"] = ids.parentTurnID
	}
	if ids.rootTurnID != "" {
		existing["root_turn_id"] = ids.rootTurnID
	}
	if ids.contextWindowID != "" {
		existing["context_window_id"] = ids.contextWindowID
	}

	fields := map[string]any{
		"installation_id":         ids.installationID,
		"session_id":              ids.sessionID,
		"thread_id":               ids.threadID,
		"turn_id":                 ids.turnID,
		"window_id":               ids.windowID,
		"turn_started_at_unix_ms": ids.turnStartedAtUnixMS,
	}
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		fields["parent_turn_id"] = ids.parentTurnID
	}
	if ids.rootTurnID != "" {
		fields["root_turn_id"] = ids.rootTurnID
	}
	if ids.mode == codexFingerprintCockpit && ids.promptCacheKey != "" {
		fields["prompt_cache_key"] = ids.promptCacheKey
	}
	if ids.contextWindowID != "" {
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
	if ids.rootTurnID != "" && ids.rootTurnIDInBody {
		updated, err = sjson.SetBytes(updated, "root_turn_id", ids.rootTurnID)
		if err != nil {
			return body, false, fmt.Errorf("splice converged root_turn_id: %w", err)
		}
	}
	if ids.contextWindowID != "" && ids.contextWindowIDInBody {
		updated, err = sjson.SetBytes(updated, "context_window_id", ids.contextWindowID)
		if err != nil {
			return body, false, fmt.Errorf("splice converged context_window_id: %w", err)
		}
	}
	if ids.extendedTurnIdentity && ids.parentTurnID != "" {
		updated, err = sjson.SetBytes(updated, "parent_turn_id", ids.parentTurnID)
		if err != nil {
			return body, false, fmt.Errorf("splice converged parent_turn_id: %w", err)
		}
	}
	if !ids.extendedTurnIdentity {
		for _, path := range []string{"parent_turn_id", "root_turn_id", "context_window_id"} {
			updated, err = sjson.DeleteBytes(updated, path)
			if err != nil {
				return body, false, fmt.Errorf("remove unsupported %s: %w", path, err)
			}
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
