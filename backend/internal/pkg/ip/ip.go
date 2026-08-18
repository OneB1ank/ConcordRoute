// Package ip 提供客户端 IP 地址提取工具。
package ip

import (
	"net"
	"strings"

	"github.com/gin-gonic/gin"
)

// 该键已经写入现有实例的设置表，重命名会让升级后的进程读不到原配置。
const forwardedIPSettingsKey = "tokenrouter.forwarded_ip_settings"

type forwardedIPSettings struct {
	trustForwarded bool
	headers        []string
}

// SetForwardedIPSettings 为当前请求保存转发 IP 模式和自定义请求头列表的快照。
func SetForwardedIPSettings(c *gin.Context, enabled bool, headers []string) {
	if c == nil {
		return
	}
	c.Set(forwardedIPSettingsKey, forwardedIPSettings{
		trustForwarded: enabled,
		headers:        append([]string(nil), headers...),
	})
}

// SetLegacyForwardedIPTrust 记录当前请求是否由原始转发头覆盖 Gin 的可信代理链。
func SetLegacyForwardedIPTrust(c *gin.Context, enabled bool) {
	SetForwardedIPSettings(c, enabled, nil)
}

func requestForwardedIPSettings(c *gin.Context) (forwardedIPSettings, bool) {
	if c == nil {
		return forwardedIPSettings{}, false
	}
	value, ok := c.Get(forwardedIPSettingsKey)
	if !ok {
		return forwardedIPSettings{}, false
	}
	settings, ok := value.(forwardedIPSettings)
	return settings, ok
}

func requestUsesLegacyForwardedIPTrust(c *gin.Context) bool {
	settings, ok := requestForwardedIPSettings(c)
	return !ok || settings.trustForwarded
}

// GetClientIP 按可信代理加固前的转发头优先级解析客户端地址。
// 该方法用于兼容请求元数据、用量和错误日志；安全敏感调用应使用
// GetTrustedClientIP 或 GetSecurityClientIP。
func GetClientIP(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if !requestUsesLegacyForwardedIPTrust(c) {
		return GetTrustedClientIP(c)
	}

	settings, _ := requestForwardedIPSettings(c)
	customIP, customFallback := resolveCustomForwardedClientIP(c, settings.headers)
	if customIP != "" {
		return customIP
	}

	// 保留现有反向代理部署依赖的历史优先级；当 XFF 中存在公网地址时，
	// 跳过代理写入 X-Real-IP 的 Docker/Nginx 内部网桥地址。
	legacyIP, legacyFallback := resolveLegacyForwardedHeaderIP(c)
	if legacyIP != "" {
		return legacyIP
	}
	if customFallback != "" {
		return customFallback
	}
	if legacyFallback != "" {
		return legacyFallback
	}
	return normalizeIP(c.ClientIP())
}

func resolveCustomForwardedClientIP(c *gin.Context, headers []string) (string, string) {
	if c == nil {
		return "", ""
	}
	var fallback string
	for _, header := range headers {
		for _, value := range c.Request.Header.Values(header) {
			for _, candidate := range strings.Split(value, ",") {
				parsed := net.ParseIP(strings.TrimSpace(candidate))
				if parsed == nil {
					continue
				}
				normalized := parsed.String()
				if isPrivateIP(normalized) {
					if fallback == "" {
						fallback = normalized
					}
					continue
				}
				return normalized, fallback
			}
		}
	}
	return "", fallback
}

func resolveLegacyForwardedHeaderIP(c *gin.Context) (string, string) {
	var fallback string
	if forwarded := normalizeValidIP(c.GetHeader("CF-Connecting-IP")); forwarded != "" {
		fallback = forwarded
		if !isPrivateIP(forwarded) {
			return forwarded, fallback
		}
	}
	if realIP := normalizeValidIP(c.GetHeader("X-Real-IP")); realIP != "" {
		if fallback == "" {
			fallback = realIP
		}
		if !isPrivateIP(realIP) {
			return realIP, fallback
		}
	}
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		for _, candidate := range ips {
			candidate = normalizeValidIP(candidate)
			if candidate != "" && !isPrivateIP(candidate) {
				return candidate, fallback
			}
		}
		if fallback == "" {
			for _, candidate := range ips {
				if candidate = normalizeValidIP(candidate); candidate != "" {
					fallback = candidate
					break
				}
			}
		}
	}
	return "", fallback
}

// GetTrustedClientIP 从 Gin 的可信代理解析链提取客户端 IP。
// 该方法依赖 gin.Engine.SetTrustedProxies 配置，不会优先直接信任原始转发头值。
// 适用于 ACL / 风控等安全敏感场景。
func GetTrustedClientIP(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return normalizeIP(c.ClientIP())
}

// GetSecurityClientIP 返回安全敏感链路使用的客户端地址。
// 兼容模式开启时由原始转发头接管解析；关闭时以 Gin 的
// server.trusted_proxies 可信代理链为唯一权威来源。
func GetSecurityClientIP(c *gin.Context, trustForwarded bool) string {
	if requestSettings, ok := requestForwardedIPSettings(c); ok {
		trustForwarded = requestSettings.trustForwarded
	}
	if trustForwarded {
		return GetClientIP(c)
	}
	return GetTrustedClientIP(c)
}

// normalizeIP 规范化 IP 地址，去除端口号和空格。
func normalizeIP(ip string) string {
	ip = strings.TrimSpace(ip)
	// 移除端口号（如 "192.168.1.1:8080" -> "192.168.1.1"）
	if host, _, err := net.SplitHostPort(ip); err == nil {
		return host
	}
	return ip
}

// normalizeValidIP 规范化并验证代理头中的候选值，避免把 unknown、主机名等非法值传给安全服务。
func normalizeValidIP(value string) string {
	normalized := normalizeIP(value)
	parsed := net.ParseIP(normalized)
	if parsed == nil {
		return ""
	}
	return parsed.String()
}

// privateNets 包含从旧 X-Forwarded-For 链选择公网地址时跳过的私网和回环网段。
var privateNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"127.0.0.0/8",
		"::1/128",
		"fc00::/7",
	} {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("invalid CIDR: " + cidr)
		}
		privateNets = append(privateNets, block)
	}
}

// CompiledIPRules 表示预编译的 IP 匹配规则。
// PatternCount 记录原始规则数量，用于保留“规则存在但全无效”时的行为语义。
type CompiledIPRules struct {
	CIDRs        []*net.IPNet
	IPs          []net.IP
	PatternCount int
}

// CompileIPRules 将 IP/CIDR 字符串规则预编译为可复用结构。
// 非法规则会被忽略，但 PatternCount 会保留原始规则条数。
func CompileIPRules(patterns []string) *CompiledIPRules {
	compiled := &CompiledIPRules{
		CIDRs:        make([]*net.IPNet, 0, len(patterns)),
		IPs:          make([]net.IP, 0, len(patterns)),
		PatternCount: len(patterns),
	}
	for _, pattern := range patterns {
		normalized := strings.TrimSpace(pattern)
		if normalized == "" {
			continue
		}
		if strings.Contains(normalized, "/") {
			_, cidr, err := net.ParseCIDR(normalized)
			if err != nil || cidr == nil {
				continue
			}
			compiled.CIDRs = append(compiled.CIDRs, cidr)
			continue
		}
		parsedIP := net.ParseIP(normalized)
		if parsedIP == nil {
			continue
		}
		compiled.IPs = append(compiled.IPs, parsedIP)
	}
	return compiled
}

func matchesCompiledRules(parsedIP net.IP, rules *CompiledIPRules) bool {
	if parsedIP == nil || rules == nil {
		return false
	}
	for _, cidr := range rules.CIDRs {
		if cidr.Contains(parsedIP) {
			return true
		}
	}
	for _, ruleIP := range rules.IPs {
		if parsedIP.Equal(ruleIP) {
			return true
		}
	}
	return false
}

func isPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, block := range privateNets {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// MatchesPattern 检查 IP 是否匹配指定的模式（支持单个 IP 或 CIDR）。
// pattern 可以是：
// - 单个 IP: "192.168.1.100"
// - CIDR 范围: "192.168.1.0/24"
func MatchesPattern(clientIP, pattern string) bool {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}

	// 尝试解析为 CIDR
	if strings.Contains(pattern, "/") {
		_, cidr, err := net.ParseCIDR(pattern)
		if err != nil {
			return false
		}
		return cidr.Contains(ip)
	}

	// 作为单个 IP 处理
	patternIP := net.ParseIP(pattern)
	if patternIP == nil {
		return false
	}
	return ip.Equal(patternIP)
}

// MatchesAnyPattern 检查 IP 是否匹配任意一个模式。
func MatchesAnyPattern(clientIP string, patterns []string) bool {
	for _, pattern := range patterns {
		if MatchesPattern(clientIP, pattern) {
			return true
		}
	}
	return false
}

// CheckIPRestriction 检查 IP 是否被 API Key 的 IP 限制允许。
// 返回值：(是否允许, 拒绝原因)
// 逻辑：
// 1. 先检查黑名单，如果在黑名单中则直接拒绝
// 2. 如果白名单不为空，IP 必须在白名单中
// 3. 如果白名单为空，允许访问（除非被黑名单拒绝）
func CheckIPRestriction(clientIP string, whitelist, blacklist []string) (bool, string) {
	return CheckIPRestrictionWithCompiledRules(
		clientIP,
		CompileIPRules(whitelist),
		CompileIPRules(blacklist),
	)
}

// CheckIPRestrictionWithCompiledRules 使用预编译规则检查 IP 是否允许访问。
func CheckIPRestrictionWithCompiledRules(clientIP string, whitelist, blacklist *CompiledIPRules) (bool, string) {
	// 规范化 IP
	clientIP = normalizeIP(clientIP)
	if clientIP == "" {
		return false, "access denied"
	}
	parsedIP := net.ParseIP(clientIP)
	if parsedIP == nil {
		return false, "access denied"
	}

	// 1. 检查黑名单
	if blacklist != nil && blacklist.PatternCount > 0 && matchesCompiledRules(parsedIP, blacklist) {
		return false, "access denied"
	}

	// 2. 检查白名单（如果设置了白名单，IP 必须在其中）
	if whitelist != nil && whitelist.PatternCount > 0 && !matchesCompiledRules(parsedIP, whitelist) {
		return false, "access denied"
	}

	return true, ""
}

// ValidateIPPattern 验证 IP 或 CIDR 格式是否有效。
func ValidateIPPattern(pattern string) bool {
	if strings.Contains(pattern, "/") {
		_, _, err := net.ParseCIDR(pattern)
		return err == nil
	}
	return net.ParseIP(pattern) != nil
}

// ValidateIPPatterns 验证多个 IP 或 CIDR 格式。
// 返回无效的模式列表。
func ValidateIPPatterns(patterns []string) []string {
	var invalid []string
	for _, p := range patterns {
		if !ValidateIPPattern(p) {
			invalid = append(invalid, p)
		}
	}
	return invalid
}
