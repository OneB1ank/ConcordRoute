package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultOpenAIOAuthImportDefaultsLeavesFingerprintOff(t *testing.T) {
	defaults := DefaultOpenAIOAuthImportDefaults()
	require.NotContains(t, defaults.Extra, codexFingerprintModeExtraKey)
}

func TestFillOpenAIOAuthImportDefaultsPreservesExplicitFingerprintMode(t *testing.T) {
	missing := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{})
	require.NotContains(t, missing.Extra, codexFingerprintModeExtraKey)

	explicit := fillOpenAIOAuthImportDefaults(&OpenAIOAuthImportDefaults{
		Extra: map[string]any{codexFingerprintModeExtraKey: "cockpit"},
	})
	require.Equal(t, "cockpit", explicit.Extra[codexFingerprintModeExtraKey])
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
