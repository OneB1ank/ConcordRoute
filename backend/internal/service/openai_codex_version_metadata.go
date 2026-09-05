package service

import (
	"bytes"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexVersionFromOutboundHeaders 只读取最终 UA 首段的引擎版本，不取桌面构建号。
func codexVersionFromOutboundHeaders(h http.Header) string {
	return NormalizeCodexClientVersion(openai.CodexUserAgentVersion(h.Get("User-Agent")))
}

// rewriteCodexVersionMetadata 只替换 JSON 对象中已有的字符串 codex_version。
// 未知字段、数值精度与其他字节保持原样；解析失败、缺失或类型异常时保留输入。
func rewriteCodexVersionMetadata(raw []byte, version string) ([]byte, bool) {
	version = NormalizeCodexClientVersion(version)
	if version == "" || !gjson.ValidBytes(raw) || !gjson.ParseBytes(raw).IsObject() {
		return raw, false
	}
	current := gjson.GetBytes(raw, "codex_version")
	if current.Type != gjson.String || current.String() == version {
		return raw, false
	}
	updated, err := sjson.SetBytes(raw, "codex_version", version)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// applyCodexVersionTurnMetadataHeader 处理真正的回合元数据头，不补造独立版本头。
func applyCodexVersionTurnMetadataHeader(h http.Header) {
	version := codexVersionFromOutboundHeaders(h)
	if version == "" {
		return
	}
	key := http.CanonicalHeaderKey("x-codex-turn-metadata")
	values := h.Values(key)
	for i, raw := range values {
		if updated, changed := rewriteCodexVersionMetadata([]byte(raw), version); changed {
			// 避免改写与原入站头共享的底层切片。
			next := append([]string(nil), values...)
			next[i] = string(updated)
			values = next
			h[key] = values
		}
	}
}

// rewriteCodexVersionClientMetadataRaw 只定位 client_metadata 小对象中的版本。
// 不遍历 input、tools、工具返回文本或会话/缓存字段，也不新增缺失的元数据。
func rewriteCodexVersionClientMetadataRaw(body []byte, version string) []byte {
	version = NormalizeCodexClientVersion(version)
	if version == "" || !bytes.Contains(body, []byte("codex_version")) || !gjson.ValidBytes(body) {
		return body
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return body
	}
	updated, changed := rewriteCodexVersionMetadata([]byte(metadata.Raw), version)
	embedded := gjson.GetBytes(updated, "x-codex-turn-metadata")
	if embedded.Type == gjson.String {
		if next, ok := rewriteCodexVersionMetadata([]byte(embedded.String()), version); ok {
			var err error
			updated, err = sjson.SetBytes(updated, "x-codex-turn-metadata", string(next))
			if err != nil {
				return body
			}
			changed = true
		}
	}
	if !changed {
		return body
	}
	result, err := sjson.SetRawBytes(body, "client_metadata", updated)
	if err != nil {
		return body
	}
	return result
}

// applyCodexVersionClientMetadata 在 WS 已解码体中执行相同规则。
// 仅在确有修改时复制元数据，防止重试或并行请求共享 map 被原地污染。
func applyCodexVersionClientMetadata(payload map[string]any, version string) {
	version = NormalizeCodexClientVersion(version)
	if version == "" || payload == nil {
		return
	}
	var original map[string]any
	switch metadata := payload["client_metadata"].(type) {
	case map[string]any:
		original = metadata
	case map[string]string:
		original = make(map[string]any, len(metadata))
		for key, value := range metadata {
			original[key] = value
		}
	default:
		return
	}
	updates := map[string]any{}
	if current, ok := original["codex_version"].(string); ok && current != version {
		updates["codex_version"] = version
	}
	if embedded, ok := original["x-codex-turn-metadata"].(string); ok {
		if updated, changed := rewriteCodexVersionMetadata([]byte(embedded), version); changed {
			updates["x-codex-turn-metadata"] = string(updated)
		}
	}
	if len(updates) == 0 {
		return
	}
	next := maps.Clone(original)
	maps.Copy(next, updates)
	payload["client_metadata"] = next
}

// applyCodexVersionRequestBody 在最终 UA 选定后更新 OAuth 请求的已有版本元数据。
// 与 HTTP/WS 的 UA 候选优先级共用同一入口，API Key 与其他平台不受影响。
func (s *OpenAIGatewayService) applyCodexVersionRequestBody(req *http.Request, account *Account, body []byte, routerMatch ...TLSFingerprintRouterMatchResult) {
	if req == nil || account == nil || account.Type != AccountTypeOAuth || account.Platform != PlatformOpenAI {
		return
	}
	// 已收口的请求直接读取最终 UA，避免再次解析全局设置造成版本漂移。
	version := codexVersionFromOutboundHeaders(req.Header)
	if strings.TrimSpace(req.Header.Get("originator")) == "" {
		// Messages 桥稍后用显式 Router/账号候选恢复身份，不保留仅 Profile
		// 路由中的中间 UA；此处必须采用与恢复点完全相同的候选规则。
		version = resolveCodexOutboundIdentity(s.codexIdentityOverrideUA(account, routerMatch...)).version
	}
	updated := rewriteCodexVersionClientMetadataRaw(body, version)
	if bytes.Equal(body, updated) {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(updated))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(updated)), nil
	}
	req.ContentLength = int64(len(updated))
	req.Header.Del("Content-Length")
}
