package service

import (
	"net/http"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func withCodexCanonicalUA(t *testing.T, ua string) {
	t.Helper()
	SetCodexCanonicalUserAgentResolver(func() string { return ua })
	t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(nil) })
}

func TestCodexDefaultIdentityUsesWindowsDesktop(t *testing.T) {
	SetCodexCanonicalUserAgentResolver(nil)

	identity := resolveCodexOutboundIdentity("")
	require.Equal(t, DefaultOpenAICodexUserAgent, identity.userAgent)
	require.Equal(t, "Codex Desktop", identity.originator)
	require.Equal(t, "0.153.0", identity.version)
	require.Contains(t, identity.userAgent, "(Codex Desktop; 26.901.20858)")
}

func TestCodexTLSProfileOnlyRouteKeepsCanonicalDesktopIdentity(t *testing.T) {
	withCodexCanonicalUA(t, DefaultOpenAICodexUserAgent)

	h := make(http.Header)
	h.Set("originator", "codex_exec")
	h.Set("user-agent", "codex_exec/0.145.0 (Windows 10.0.26200; x86_64) dumb (codex_exec; 0.145.0)")
	enforceCodexIdentityHeadersWithUA(h, "")

	require.Equal(t, DefaultOpenAICodexUserAgent, h.Get("user-agent"))
	require.Equal(t, "Codex Desktop", h.Get("originator"))
	require.Equal(t, "0.153.0", h.Get("version"))
}

func TestEnsureCodexIdentityHeadersUsesCanonicalIdentity(t *testing.T) {
	const macUA = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, macUA)

	h := make(http.Header)
	ensureCodexIdentityHeaders(h)
	enforceCodexIdentityHeaders(h)

	require.Equal(t, "codex-tui", h.Get("originator"))
	require.Equal(t, macUA, h.Get("user-agent"))
	require.Equal(t, "0.200.1", h.Get("version"))
	require.Equal(t, "responses=experimental", h.Get("OpenAI-Beta"))
}

func TestResolveCodexOutboundIdentityPreservesConfiguredFingerprint(t *testing.T) {
	const canonical = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	const routed = "codex_cli_rs/0.150.0 (Mac OS X 14.7; arm64) iTerm2 (codex_cli_rs; 0.150.0)"
	withCodexCanonicalUA(t, canonical)

	identity := resolveCodexOutboundIdentity(routed)
	require.Equal(t, "codex_cli_rs", identity.originator)
	require.Equal(t, "0.150.0", identity.version)
	require.Equal(t,
		"codex_cli_rs/0.150.0 (Mac OS X 14.7; arm64) iTerm2 (codex_cli_rs; 0.150.0)",
		identity.userAgent,
	)
}

func TestResolveCodexOutboundIdentityRejectsInvalidOverride(t *testing.T) {
	const canonical = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, canonical)

	identity := resolveCodexOutboundIdentity("browser-or-random-client")
	require.Equal(t, "codex-tui", identity.originator)
	require.Equal(t, canonical, identity.userAgent)
	require.Equal(t, "0.200.1", identity.version)
}

func TestResolveCodexOutboundIdentityRepairsOldConfiguredVersion(t *testing.T) {
	const oldMacUA = "codex-tui/0.91.0 (Mac OS X 14.7; arm64) Terminal.app (codex-tui; 0.91.0)"
	withCodexCanonicalUA(t, oldMacUA)

	identity := resolveCodexOutboundIdentity("")
	require.Equal(t, codexCLIVersion, identity.version)
	require.Equal(t,
		"codex-tui/"+codexCLIVersion+" (Mac OS X 14.7; arm64) Terminal.app (codex-tui; "+codexCLIVersion+")",
		identity.userAgent,
	)
}

func TestCodexCanonicalAuthIdentityOmitsInferenceVersion(t *testing.T) {
	const macUA = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	withCodexCanonicalUA(t, macUA)

	h := make(http.Header)
	h.Set("version", "stale")
	ApplyCodexCanonicalAuthIdentity(h)

	require.Equal(t, macUA, h.Get("user-agent"))
	require.Equal(t, "codex-tui", h.Get("originator"))
	require.Empty(t, h.Get("version"))
}

func TestEnforceCodexIdentityHeadersWithUA(t *testing.T) {
	const canonical = "codex-tui/0.200.1 (Mac OS X 15.6; arm64) Terminal.app (codex-tui; 0.200.1)"
	const accountUA = "codex_vscode/0.180.0 (Mac OS X 14.7; arm64) vscode (codex_vscode; 0.180.0)"
	withCodexCanonicalUA(t, canonical)

	h := make(http.Header)
	h.Set("originator", "wrong")
	h.Set("user-agent", "client-controlled/1.0")
	h.Set("version", "9.9.9")
	enforceCodexIdentityHeadersWithUA(h, accountUA)

	require.Equal(t, "codex_vscode", h.Get("originator"))
	require.Equal(t, "9.9.9", h.Get("version"))
	require.Equal(t,
		"codex_vscode/0.180.0 (Mac OS X 14.7; arm64) vscode (codex_vscode; 0.180.0)",
		h.Get("user-agent"),
	)
}

func TestEnforceCodexIdentityHeadersNoOriginatorIsNoop(t *testing.T) {
	h := make(http.Header)
	h.Set("user-agent", "third-party-client/1.0.0")

	enforceCodexIdentityHeaders(h)

	require.Empty(t, h.Get("originator"))
	require.Equal(t, "third-party-client/1.0.0", h.Get("user-agent"))
}

func TestNormalizeCodexClientVersion(t *testing.T) {
	require.Equal(t, "0.200.1-alpha.4", NormalizeCodexClientVersion(" 0.200.1-alpha.4 "))
	require.Empty(t, NormalizeCodexClientVersion("0.200.1\r\nX-Test: injected"))
	require.Empty(t, NormalizeCodexClientVersion("version-0.200.1"))
	require.Equal(t, openai.CodexDefaultOriginator, resolveCodexOutboundIdentity("").originator)
}
