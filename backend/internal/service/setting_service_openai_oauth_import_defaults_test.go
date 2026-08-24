package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultOpenAIOAuthImportDefaultsUsesCockpitFingerprint(t *testing.T) {
	defaults := DefaultOpenAIOAuthImportDefaults()
	require.Equal(t, "cockpit", defaults.Extra[codexFingerprintModeExtraKey])
}

func TestFillOpenAIOAuthImportDefaultsPreservesExplicitFingerprintMode(t *testing.T) {
	missing := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{})
	require.Equal(t, "cockpit", missing.Extra[codexFingerprintModeExtraKey])

	explicit := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{
		Extra: map[string]any{codexFingerprintModeExtraKey: "off"},
	})
	require.Equal(t, "off", explicit.Extra[codexFingerprintModeExtraKey])
}

func TestMergeMissingOpenAIOAuthImportDefaultsKeepsGenericExtraMerging(t *testing.T) {
	target := map[string]any{
		"explicit": "saved",
	}
	mergeMissingOpenAIOAuthImportDefaults(&target, map[string]any{
		"explicit":       "default",
		"future_default": true,
	})

	require.Equal(t, "saved", target["explicit"])
	require.Equal(t, true, target["future_default"])
}
