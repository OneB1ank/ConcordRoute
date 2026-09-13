package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIStandardHeadersDoNotAcceptUntrustedAttestation(t *testing.T) {
	require.False(t, openaiAllowedHeaders["x-oai-attestation"])
	require.False(t, openaiPassthroughAllowedHeaders["x-oai-attestation"])
}
