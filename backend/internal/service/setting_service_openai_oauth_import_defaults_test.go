package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultOpenAIOAuthImportDefaultsUsesCockpitFingerprint(t *testing.T) {
	defaults := DefaultOpenAIOAuthImportDefaults()
	require.Equal(t, "cockpit", defaults.Extra[codexFingerprintModeExtraKey])
}

func TestFillOpenAIOAuthImportDefaultsMergesCockpitWithoutOverwritingExplicitMode(t *testing.T) {
	missing := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{})
	require.Equal(t, "cockpit", missing.Extra[codexFingerprintModeExtraKey])

	explicit := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{
		Extra: map[string]any{codexFingerprintModeExtraKey: "session"},
	})
	require.Equal(t, "session", explicit.Extra[codexFingerprintModeExtraKey])
}
