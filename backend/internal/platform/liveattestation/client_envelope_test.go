package liveattestation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeClientEnvelopeAcceptsOpaqueClientToken(t *testing.T) {
	got, err := NormalizeClientEnvelope(`{"s":0,"t":"  opaque-client-owned-token  ","v":1}`)
	require.NoError(t, err)
	require.Equal(t, `{"v":1,"s":0,"t":"  opaque-client-owned-token  "}`, got)
}

func TestNormalizeClientEnvelopeAcceptsClientFailureStatuses(t *testing.T) {
	for _, status := range []int{1, 2, 3, 4} {
		got, err := NormalizeClientEnvelope(fmt.Sprintf(`{"v":1,"s":%d}`, status))
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf(`{"v":1,"s":%d}`, status), got)
	}
}

func TestNormalizeClientEnvelopeRejectsMalformedOrOversizedValues(t *testing.T) {
	for _, raw := range []string{
		`{"v":1,"s":0,"t":""}`,
		`{"v":1,"s":1,"t":"v1.should-not-be-present"}`,
		`{"v":2,"s":0,"t":"v1.token"}`,
		`{"v":1,"s":0,"t":"v1.` + string(make([]byte, maxClientAttestationTokenBytes)) + `"}`,
	} {
		_, err := NormalizeClientEnvelope(raw)
		require.Error(t, err, raw)
	}
}
