package liveattestation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseInitializeRequestTracksAttestationCapability(t *testing.T) {
	got, err := ParseInitializeRequest([]byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, got)

	got, err = ParseInitializeRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"capabilities":{"experimentalApi":true}}}`))
	require.NoError(t, err)
	require.False(t, got)

	got, err = ParseInitializeRequest([]byte(`{"method":"thread/start"}`))
	require.NoError(t, err)
	require.False(t, got)
}

func TestBuildAndParseAttestationGenerateRoundTrip(t *testing.T) {
	request, err := BuildAttestationGenerateRequest(17)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(request, &decoded))
	_, hasJSONRPC := decoded["jsonrpc"]
	require.False(t, hasJSONRPC, "official app-server wire format omits jsonrpc")
	require.Equal(t, "attestation/generate", decoded["method"])
	require.Equal(t, float64(17), decoded["id"])

	id, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":{"token":"v1.client-token"}}`))
	require.NoError(t, err)
	require.Equal(t, `17`, string(id))
	require.Equal(t, "v1.client-token", token)
}

func TestAppServerProtocolAcceptsOfficialFramesWithoutJSONRPCVersion(t *testing.T) {
	capability, err := ParseInitializeRequest([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, capability)

	_, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":{"token":"v1.opaque"}}`))
	require.NoError(t, err)
	require.Equal(t, "v1.opaque", token)
}

func TestParseAttestationGenerateResponsePreservesOpaqueToken(t *testing.T) {
	_, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":{"headerValue":"  opaque-token-without-prefix  "}}`))
	require.NoError(t, err)
	require.Equal(t, "  opaque-token-without-prefix  ", token)
}

func TestParseAttestationGenerateResponseRejectsFailures(t *testing.T) {
	_, _, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"error":{"code":-1,"message":"failed"}}`))
	require.ErrorIs(t, err, ErrAttestationClientRequestFailed)
	for _, raw := range []string{
		`{"id":17,"result":{"headerValue":""}}`,
		`{"id":17,"result":null}`,
	} {
		_, _, err := ParseAttestationGenerateResponse([]byte(raw))
		require.Error(t, err, raw)
	}
}

// 原生 token 优先；只有字段缺失才接受旧别名，不掩盖坏值或字段冲突。
func TestParseAttestationGenerateResponseNativeTokenAndLegacyAlias(t *testing.T) {
	for _, result := range []string{
		`{"token":"opaque value"}`,
		`{"headerValue":"opaque value"}`,
		`{"token":"opaque value","headerValue":"opaque value"}`,
	} {
		id, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":` + result + `}`))
		require.NoError(t, err)
		require.Equal(t, "17", string(id))
		require.Equal(t, "opaque value", token)
	}
}

func TestParseAttestationGenerateResponseRejectsInvalidOrConflictingFields(t *testing.T) {
	for _, raw := range []string{
		`{"id":17,"result":{"token":""}}`,
		`{"id":17,"result":{"token":17}}`,
		`{"id":17,"result":{"token":null}}`,
		`{"id":17,"result":{"token":"","headerValue":"v1.legacy"}}`,
		`{"id":17,"result":{"token":null,"headerValue":"v1.legacy"}}`,
		`{"id":17,"result":{"token":17,"headerValue":"v1.legacy"}}`,
		`{"id":17,"result":{"token":"v1.native","headerValue":"v1.legacy"}}`,
		`{"id":17,"result":{"token":"v1.native","headerValue":null}}`,
		`{"id":17,"result":{"headerValue":17}}`,
		`{"id":17,"result":{"headerValue":null}}`,
		`{"id":17,"result":{"token":"` + strings.Repeat("x", maxClientAttestationTokenBytes+1) + `"}}`,
	} {
		_, _, err := ParseAttestationGenerateResponse([]byte(raw))
		require.Error(t, err, raw)
	}
}
