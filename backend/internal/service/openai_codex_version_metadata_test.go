package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 验证只改已有回合元数据中的引擎版本，不凭空增加独立 HTTP 头。
func TestCodexVersionMetadataDoesNotInventHTTPHeader(t *testing.T) {
	const ua = "Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.901.41123)"
	h := make(http.Header)
	h.Set("Originator", "client")
	h.Set("Version", "0.145.0")
	h.Set("X-Codex-Turn-Metadata", `{"codex_version":"0.145.0","tool":"shell","turn_id":"turn-a","future":9007199254740993}`)

	enforceCodexIdentityHeadersWithUA(h, ua)

	require.Equal(t, "0.153.3", h.Get("Version"))
	require.Equal(t, ua, h.Get("User-Agent"))
	require.Empty(t, h.Get("codex_version"), "codex_version 是元数据字段，不是新增 HTTP 头")
	require.Equal(t, `{"codex_version":"0.153.3","tool":"shell","turn_id":"turn-a","future":9007199254740993}`, h.Get("X-Codex-Turn-Metadata"))
}

// 覆盖旧元数据的兼容边界，所有无效输入均逐字节保留。
func TestCodexVersionMetadataRewriteBoundaries(t *testing.T) {
	const version = "0.153.3"
	for _, raw := range []string{
		"", "not-json", `{"codex_version":`, `[]`, `null`, `"text"`,
		`{}`, `{"codex_version":null}`, `{"codex_version":153}`, `{"codex_version":[]}`,
		`{"codex_version":{}}`, `{"codex_version":"0.153.3"}`, `{"tool":"shell"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			got, changed := rewriteCodexVersionMetadata([]byte(raw), version)
			require.False(t, changed)
			require.Equal(t, raw, string(got))
		})
	}
	const original = `{ "codex_version" : "0.145.0", "tool":"shell", "future":9007199254740993, "nested":{"codex_version":"keep"} }`
	got, changed := rewriteCodexVersionMetadata([]byte(original), version)
	require.True(t, changed)
	require.Equal(t, strings.Replace(original, "0.145.0", version, 1), string(got))
	for _, invalid := range []string{"", "invalid", "0.153.3\r\nX-Test: injected"} {
		got, changed = rewriteCodexVersionMetadata([]byte(original), invalid)
		require.False(t, changed)
		require.Equal(t, original, string(got))
	}
}

// 多值头不污染共享入站切片；异常 UA 不使用桌面后缀或客户端头作为猜测值。
func TestCodexVersionMetadataHeadersPreserveSource(t *testing.T) {
	const raw = `{"codex_version":"0.145.0","session_id":"same"}`
	values := []string{raw, `{"codex_version":null}`, raw}
	h := http.Header{"X-Codex-Turn-Metadata": values}
	h.Set("User-Agent", "Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.901.41123)")
	applyCodexVersionTurnMetadataHeader(h)
	require.Equal(t, []string{raw, `{"codex_version":null}`, raw}, values)
	require.Equal(t, []string{strings.Replace(raw, "0.145.0", "0.153.3", 1), values[1], strings.Replace(raw, "0.145.0", "0.153.3", 1)}, h.Values("X-Codex-Turn-Metadata"))
	h.Set("User-Agent", "Codex Desktop/invalid (Codex Desktop; 26.901.41123)")
	h.Set("X-Codex-Turn-Metadata", raw)
	applyCodexVersionTurnMetadataHeader(h)
	require.Equal(t, raw, h.Get("X-Codex-Turn-Metadata"))
	applyCodexVersionTurnMetadataHeader(nil)
}

const codexVersionMetadataBody = `{
    "model":"gpt-5.6-sol", "prompt_cache_key":"cache-a", "session_id":"session-a",
    "thread_id":"thread-a", "window_id":"thread-a:2", "context_window_id":"window-a",
    "client_metadata":{"codex_version":"0.145.0","future":9007199254740993,"session_id":"session-a","thread_id":"thread-a","turn_id":"turn-a","root_turn_id":"root-a","parent_turn_id":"parent-a","window_id":"thread-a:2","context_window_id":"window-a","prompt_cache_key":"cache-a","x-codex-turn-metadata":"{\"codex_version\":\"0.145.0\",\"tool\":\"shell\",\"future\":9007199254740993}"},
    "input":[{"type":"function_call_output","call_id":"call-a","output":"codex_version 0.145.0"}],
    "tools":[{"type":"function","name":"shell","description":"codex_version 0.145.0"}],
    "metadata":{"codex_version":"keep"}
}`

// 同时覆盖 HTTP 原始 JSON 与 WS map，验证仅改两个已存在的版本值。
func TestCodexVersionClientMetadataOnlyReplacesExistingVersions(t *testing.T) {
	want := strings.Replace(codexVersionMetadataBody, `"codex_version":"0.145.0"`, `"codex_version":"0.153.3"`, 1)
	want = strings.Replace(want, `\"codex_version\":\"0.145.0\"`, `\"codex_version\":\"0.153.3\"`, 1)
	original := []byte(codexVersionMetadataBody)
	got := rewriteCodexVersionClientMetadataRaw(original, "0.153.3")
	require.Equal(t, want, string(got))
	require.Equal(t, codexVersionMetadataBody, string(original))
	require.Equal(t, got, rewriteCodexVersionClientMetadataRaw(got, "0.153.3"))
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(codexVersionMetadataBody))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&payload))
	source, ok := payload["client_metadata"].(map[string]any)
	require.True(t, ok)
	before, err := json.Marshal(source)
	require.NoError(t, err)
	applyCodexVersionClientMetadata(payload, "0.153.3")
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	require.JSONEq(t, want, string(encoded))
	after, err := json.Marshal(source)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "9007199254740993", gjson.GetBytes(encoded, "client_metadata.future").Raw)

	stringsMeta := map[string]string{"codex_version": "0.145.0", "tool": "shell"}
	payload = map[string]any{"client_metadata": stringsMeta}
	applyCodexVersionClientMetadata(payload, "0.153.3")
	require.Equal(t, "0.145.0", stringsMeta["codex_version"])
	updated, ok := payload["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "0.153.3", updated["codex_version"])
}

// 缺失字段、异常类型和工具正文都不应触发补造元数据。
func TestCodexVersionClientMetadataNoop(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"codex_version":"0.145.0"}]}`, `{"client_metadata":null}`, `{"client_metadata":[]}`,
		`{"client_metadata":{}}`, `{"client_metadata":{"codex_version":153,"x-codex-turn-metadata":123}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"not-json"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{\"tool\":\"shell\"}"}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			require.Equal(t, raw, string(rewriteCodexVersionClientMetadataRaw([]byte(raw), "0.153.3")))
			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(raw), &payload))
			applyCodexVersionClientMetadata(payload, "0.153.3")
			got, err := json.Marshal(payload)
			require.NoError(t, err)
			require.JSONEq(t, raw, string(got))
		})
	}
	malformed := []byte(`{"client_metadata":{"codex_version":"0.145.0"}`)
	require.Equal(t, malformed, rewriteCodexVersionClientMetadataRaw(malformed, "0.153.3"))
	applyCodexVersionClientMetadata(nil, "0.153.3")
}

// 使用真实构造器检查最终发送内容、重试体和长度，覆盖普通、透传和两类搜索路径。
func TestCodexVersionRequestBuilders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const ua = "Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.901.41123)"
	withCodexCanonicalUA(t, DefaultOpenAICodexUserAgent)
	for _, mode := range []string{"http", "passthrough", "alpha", "alpha_responses"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			if mode == "alpha_responses" && accountType == AccountTypeAPIKey {
				continue
			}
			t.Run(mode+"/"+accountType, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				body := []byte(codexVersionMetadataBody)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Originator", "codex-tui")
				c.Request.Header.Set("User-Agent", "codex-tui/0.145.0")
				c.Request.Header.Set("Version", "0.145.0")
				c.Request.Header.Set("X-Codex-Turn-Metadata", `{"codex_version":"0.145.0","tool":"shell","session_id":"session-a"}`)
				account := &Account{ID: 42, Platform: PlatformOpenAI, Type: accountType, Credentials: map[string]any{"user_agent": ua}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				var req *http.Request
				var err error
				switch mode {
				case "http":
					req, err = svc.buildUpstreamRequest(context.Background(), c, account, body, "token", false, "", true)
				case "passthrough":
					req, err = svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "token")
				case "alpha":
					req, err = svc.buildOpenAIAlphaSearchRequest(context.Background(), c, account, body, "token")
				case "alpha_responses":
					req, err = svc.buildOpenAIAlphaSearchResponsesWebSearchRequest(context.Background(), c, account, body, body, "token")
				}
				require.NoError(t, err)
				got, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.NoError(t, req.Body.Close())
				expected := body
				if accountType == AccountTypeOAuth {
					expected = rewriteCodexVersionClientMetadataRaw(body, "0.153.3")
					require.Equal(t, ua, req.Header.Get("User-Agent"))
					require.Equal(t, "0.153.3", req.Header.Get("Version"))
					require.Equal(t, `{"codex_version":"0.153.3","tool":"shell","session_id":"session-a"}`, req.Header.Get("X-Codex-Turn-Metadata"))
					require.Empty(t, req.Header.Get("codex_version"))
				}
				require.Equal(t, string(expected), string(got))
				require.Equal(t, int64(len(got)), req.ContentLength)
				require.NotNil(t, req.GetBody)
				retry, err := req.GetBody()
				require.NoError(t, err)
				retried, err := io.ReadAll(retry)
				require.NoError(t, err)
				require.NoError(t, retry.Close())
				require.Equal(t, got, retried)
				require.Equal(t, "0.145.0", gjson.Get(c.Request.Header.Get("X-Codex-Turn-Metadata"), "codex_version").String())
			})
		}
	}
}

// 账号、路由器与全局设置以最终选中的 UA 为准，不误用桌面构建号。
func TestCodexVersionRequestUsesFinalUAPriority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withCodexCanonicalUA(t, DefaultOpenAICodexUserAgent)
	for _, tc := range []struct {
		name      string
		accountUA string
		match     TLSFingerprintRouterMatchResult
		want      string
	}{
		{name: "global", want: "0.153.4"},
		{name: "account", accountUA: "codex-tui/0.153.3", want: "0.153.3"},
		{name: "router", accountUA: "codex-tui/0.153.3", match: TLSFingerprintRouterMatchResult{Matched: true, UpstreamUserAgent: "codex-tui/0.200.1"}, want: "0.200.1"},
		{name: "profile_only", match: TLSFingerprintRouterMatchResult{Matched: true}, want: "0.180.0"},
		{name: "invalid_account", accountUA: "invalid", want: "0.153.4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "codex-tui/0.180.0")
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"user_agent": tc.accountUA}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			req, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(codexVersionMetadataBody), "token", true, "", true, tc.match)
			require.NoError(t, err)
			body, err := io.ReadAll(req.Body)
			require.NoError(t, err)
			require.NoError(t, req.Body.Close())
			require.Equal(t, tc.want, req.Header.Get("Version"))
			require.Equal(t, tc.want, gjson.GetBytes(body, "client_metadata.codex_version").String())
		})
	}
}

// Messages 桥先移除 originator，随后恢复身份；只配 TLS 模板时也保持最终版本一致。
func TestCodexVersionMessagesBridgeMatchesRestoredIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	withCodexCanonicalUA(t, DefaultOpenAICodexUserAgent)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "codex-tui/0.180.0")
	setOpenAICompatMessagesBridgeContext(c, true)
	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	match := TLSFingerprintRouterMatchResult{Matched: true}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	req, err := svc.buildUpstreamRequest(context.Background(), c, account, []byte(codexVersionMetadataBody), "token", true, "", false, match)
	require.NoError(t, err)
	require.Empty(t, req.Header.Get("Originator"))
	ensureCodexIdentityHeaders(req.Header)
	enforceCodexIdentityHeadersWithUA(req.Header, svc.codexIdentityOverrideUA(account, match))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.NoError(t, req.Body.Close())
	require.Equal(t, req.Header.Get("Version"), gjson.GetBytes(body, "client_metadata.codex_version").String())
}
