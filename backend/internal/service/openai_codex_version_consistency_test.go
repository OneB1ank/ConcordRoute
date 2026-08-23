//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestCodexVersionConstants_Consistency(t *testing.T) {
	require.True(t, strings.Contains(codexCLIUserAgent, openai.CodexDefaultOriginator+"/"+codexCLIVersion),
		"codexCLIUserAgent must embed codexCLIVersion")

	require.Equal(t, DefaultOpenAICodexUserAgent, codexCLIUserAgent)
	require.Contains(t, DefaultOpenAICodexUserAgent, "(Windows 10.0.26200; x86_64)")
	require.Contains(t, DefaultOpenAICodexUserAgent, "(Codex Desktop; 26.818.41509)")
	require.Equal(t, codexCLIVersion, CodexCanonicalClientVersion())
}
