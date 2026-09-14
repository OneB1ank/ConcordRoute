package liveattestation

import (
	"encoding/json"
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

	id, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":{"headerValue":"v1.client-token"}}`))
	require.NoError(t, err)
	require.Equal(t, `17`, string(id))
	require.Equal(t, "v1.client-token", token)
}

func TestAppServerProtocolAcceptsOfficialFramesWithoutJSONRPCVersion(t *testing.T) {
	capability, err := ParseInitializeRequest([]byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`))
	require.NoError(t, err)
	require.True(t, capability)

	_, token, err := ParseAttestationGenerateResponse([]byte(`{"id":17,"result":{"headerValue":"v1.opaque"}}`))
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

func TestParseAttestationGenerateResponseRequiresOfficialHeaderValueField(t *testing.T) {
	for _, raw := range []string{
		`{"id":17,"result":{"token":"v1.legacy"}}`,
		`{"id":17,"result":{"headerValue":17}}`,
		`{"id":17,"result":{"headerValue":null}}`,
	} {
		_, _, err := ParseAttestationGenerateResponse([]byte(raw))
		require.Error(t, err, raw)
	}
}
