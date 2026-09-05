package service

import (
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
)

// codexUpstreamMinVersion 是 /backend-api/codex 当前接受的最低 version 头。
const codexUpstreamMinVersion = "0.151.0"

const codexClientVersionMaxLen = 64

var codexClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}(-[0-9A-Za-z.]+)?$`)

// NormalizeCodexClientVersion 校验出站版本号，避免把异常配置拼入 UA 或请求头。
func NormalizeCodexClientVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > codexClientVersionMaxLen || !codexClientVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

type codexCanonicalUserAgentResolver func() string

var (
	codexCanonicalUAMu       sync.RWMutex
	codexCanonicalUAResolver codexCanonicalUserAgentResolver
)

// SetCodexCanonicalUserAgentResolver 注入后台 OpenAI Codex UA 设置读取器。
// 读取器内部使用 SettingService 的 TTL 缓存，不会让请求热路径直接访问数据库。
func SetCodexCanonicalUserAgentResolver(resolver func() string) {
	codexCanonicalUAMu.Lock()
	defer codexCanonicalUAMu.Unlock()
	codexCanonicalUAResolver = resolver
}

func codexCanonicalUserAgent() string {
	codexCanonicalUAMu.RLock()
	resolver := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	if resolver != nil {
		if ua := strings.TrimSpace(resolver()); ua != "" {
			return ua
		}
	}
	return codexCLIUserAgent
}

type codexOutboundIdentity struct {
	userAgent  string
	originator string
	version    string
}

// resolveCodexOutboundIdentity 生成同源的 UA、originator 和 version。
// 管理员显式配置的账号/TLS Router UA 优先并作为完整身份来源；只有候选为空或
// 无效时才回退全局规范 UA，避免全局版本覆盖显式模板中的真实客户端版本。
func resolveCodexOutboundIdentity(candidateUA string) codexOutboundIdentity {
	canonicalUA := strings.TrimSpace(codexCanonicalUserAgent())
	canonicalOriginator, canonicalPairedUA, ok := openai.PairCodexClientIdentity(canonicalUA)
	if !ok {
		canonicalOriginator = openai.CodexDefaultOriginator
		canonicalPairedUA = codexCLIUserAgent
	}
	version := codexClientVersionFromUA(canonicalPairedUA)
	if rebuilt := openai.SetCodexUserAgentVersion(canonicalPairedUA, version); rebuilt != "" {
		canonicalPairedUA = rebuilt
	}

	ua := strings.TrimSpace(candidateUA)
	if ua == "" {
		return codexOutboundIdentity{
			userAgent:  canonicalPairedUA,
			originator: canonicalOriginator,
			version:    version,
		}
	}

	originator, pairedUA, ok := openai.PairCodexClientIdentity(ua)
	if !ok {
		return codexOutboundIdentity{
			userAgent:  canonicalPairedUA,
			originator: canonicalOriginator,
			version:    version,
		}
	}
	// 显式的账号/TLS Router UA 是管理员或客户端选择的身份，版本按其自身值透传；
	// 只有解析失败时才回退到全局规范身份，避免最低版本门槛静默改写显式候选。
	version = NormalizeCodexClientVersion(openai.CodexUserAgentVersion(pairedUA))
	if version == "" {
		return codexOutboundIdentity{
			userAgent:  canonicalPairedUA,
			originator: canonicalOriginator,
			version:    codexClientVersionFromUA(canonicalPairedUA),
		}
	}
	if rebuilt := openai.SetCodexUserAgentVersion(pairedUA, version); rebuilt != "" {
		pairedUA = rebuilt
	}
	return codexOutboundIdentity{userAgent: pairedUA, originator: originator, version: version}
}

func codexClientVersionFromUA(ua string) string {
	version := NormalizeCodexClientVersion(openai.CodexUserAgentVersion(ua))
	if version == "" || CompareVersions(version, codexUpstreamMinVersion) < 0 {
		return codexCLIVersion
	}
	return version
}

// CodexCanonicalUserAgent 返回当前后台配置对应的规范 Codex UA。
func CodexCanonicalUserAgent() string {
	return resolveCodexOutboundIdentity("").userAgent
}

// CodexCanonicalClientVersion 返回与规范 UA 同源的 version。
func CodexCanonicalClientVersion() string {
	return resolveCodexOutboundIdentity("").version
}

// CodexCanonicalAuthIdentity 返回凭据面使用的 UA 与 originator。
// auth.openai.com 的 Token/PAT 请求不附带推理面的 version 头。
func CodexCanonicalAuthIdentity() (userAgent, originator string) {
	identity := resolveCodexOutboundIdentity("")
	return identity.userAgent, identity.originator
}

// CodexAuthIdentityForUserAgent 使用 TLS Router/账号显式配置的完整身份；
// 候选为空或无效时才回退全局规范身份。
func CodexAuthIdentityForUserAgent(candidateUA string) (userAgent, originator string) {
	identity := resolveCodexOutboundIdentity(candidateUA)
	return identity.userAgent, identity.originator
}

func ApplyCodexCanonicalAuthIdentity(h http.Header) {
	if h == nil {
		return
	}
	userAgent, originator := CodexCanonicalAuthIdentity()
	h.Set("user-agent", userAgent)
	h.Set("originator", originator)
	h.Del("version")
	h.Del("codex_version")
}

// ensureCodexIdentityHeaders 补齐推理面所需的身份头。
func ensureCodexIdentityHeaders(h http.Header) {
	if h == nil {
		return
	}
	identity := resolveCodexOutboundIdentity("")
	if strings.TrimSpace(h.Get("user-agent")) == "" {
		h.Set("user-agent", identity.userAgent)
	}
	if strings.TrimSpace(h.Get("originator")) == "" {
		h.Set("originator", identity.originator)
	}
	// Both wire version fields are derived from the final UA. Client-provided
	// values are not used as the normal source; the final enforcement point
	// repairs them again after all header overrides have been applied.
	h.Set("version", identity.version)
	h.Set("codex_version", identity.version)
	h.Set("OpenAI-Beta", "responses=experimental")
}

func enforceCodexIdentityHeaders(h http.Header) {
	enforceCodexIdentityHeadersWithUA(h, "")
}

// enforceCodexIdentityHeadersWithUA 是推理面的最终收口点。
// 入站客户端自报身份不作为默认来源；管理员配置的账号/TLS Router UA
// 作为显式候选，从而让 HTTP、旁路探针、OAuth 与 WS 使用相同优先级规则。
// version/codex_version 是成对的引擎版本字段，正常情况下均由最终伪装 UA 派生；
// 只有 UA 解析异常时才保留经过校验的客户端值作为兼容兜底。
func enforceCodexIdentityHeadersWithUA(h http.Header, overrideUA string) {
	if h == nil || strings.TrimSpace(h.Get("originator")) == "" {
		return
	}
	identity := resolveCodexOutboundIdentity(overrideUA)
	h.Set("user-agent", identity.userAgent)
	h.Set("originator", identity.originator)
	// version and codex_version are paired engine-version fields. Derive both
	// from the final UA so a client/tool version cannot desynchronise the
	// outbound identity. If UA resolution ever fails, retain a validated
	// client value as a last-resort compatibility fallback.
	derivedVersion := NormalizeCodexClientVersion(identity.version)
	if derivedVersion != "" {
		h.Set("version", derivedVersion)
		h.Set("codex_version", derivedVersion)
		return
	}
	if fallback := NormalizeCodexClientVersion(h.Get("version")); fallback != "" {
		h.Set("version", fallback)
	}
	if fallback := NormalizeCodexClientVersion(h.Get("codex_version")); fallback != "" {
		h.Set("codex_version", fallback)
	}
}
