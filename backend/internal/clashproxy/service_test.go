package clashproxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseImportedNodesSupportsMultipleURILines(t *testing.T) {
	nodes, err := parseImportedNodes("uri", "http://user:pass@example.com:8080#first\n\nsocks5://127.0.0.1:1080#second\n")
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	require.Equal(t, "first", nodes[0].Name)
	require.Equal(t, "second", nodes[1].Name)
}

func TestNormalizeProfileInputRejectsPrivateOrInsecureTestURL(t *testing.T) {
	base := CreateProfileInput{Name: "profile", Strategy: "select", NodeIDs: []int64{1}}

	private := base
	private.TestURL = "https://127.0.0.1/generate_204"
	_, _, err := normalizeProfileInput(private)
	require.Error(t, err)

	insecure := base
	insecure.TestURL = "http://example.com/generate_204"
	_, _, err = normalizeProfileInput(insecure)
	require.Error(t, err)
}

func TestNormalizeProfileInputAppliesStableDefaults(t *testing.T) {
	input, strategy, err := normalizeProfileInput(CreateProfileInput{
		Name:    "  primary  ",
		NodeIDs: []int64{1},
	})
	require.NoError(t, err)
	require.Equal(t, "primary", input.Name)
	require.Equal(t, "select", string(strategy))
	require.Equal(t, defaultTestURL, input.TestURL)
	require.Equal(t, 300, input.IntervalSeconds)
}
