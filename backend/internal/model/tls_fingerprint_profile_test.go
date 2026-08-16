package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTLSFingerprintProfileValidateAcceptsConsistentProfile(t *testing.T) {
	profile := &TLSFingerprintProfile{
		Name:                "macOS Codex",
		CipherSuites:        []uint16{0x1301, 0x1302},
		Curves:              []uint16{29, 23},
		PointFormats:        []uint16{0},
		SignatureAlgorithms: []uint16{0x0804},
		ALPNProtocols:       []string{"h2", "http/1.1"},
		SupportedVersions:   []uint16{0x0304, 0x0303},
		KeyShareGroups:      []uint16{29},
		PSKModes:            []uint16{1},
		Extensions:          []uint16{0, 10, 11, 13, 16, 43, 45, 51},
	}

	require.NoError(t, profile.Validate())
}

func TestTLSFingerprintProfileValidateRejectsInvalidValuesEarly(t *testing.T) {
	tests := []struct {
		name    string
		profile TLSFingerprintProfile
		field   string
	}{
		{name: "point format overflow", profile: TLSFingerprintProfile{Name: "test", PointFormats: []uint16{256}}, field: "point_formats"},
		{name: "psk mode overflow", profile: TLSFingerprintProfile{Name: "test", PSKModes: []uint16{256}}, field: "psk_modes"},
		{name: "duplicate extension", profile: TLSFingerprintProfile{Name: "test", Extensions: []uint16{0, 43, 43}}, field: "extensions"},
		{name: "duplicate cipher", profile: TLSFingerprintProfile{Name: "test", CipherSuites: []uint16{0x1301, 0x1301}}, field: "cipher_suites"},
		{name: "duplicate alpn", profile: TLSFingerprintProfile{Name: "test", ALPNProtocols: []string{"h2", "h2"}}, field: "alpn_protocols"},
		{name: "empty alpn", profile: TLSFingerprintProfile{Name: "test", ALPNProtocols: []string{""}}, field: "alpn_protocols"},
		{name: "oversized alpn", profile: TLSFingerprintProfile{Name: "test", ALPNProtocols: []string{strings.Repeat("a", 256)}}, field: "alpn_protocols"},
		{name: "unsupported version", profile: TLSFingerprintProfile{Name: "test", SupportedVersions: []uint16{0x0305}}, field: "supported_versions"},
		{name: "missing alpn extension", profile: TLSFingerprintProfile{Name: "test", ALPNProtocols: []string{"h2"}, Extensions: []uint16{0, 43, 51, 13}}, field: "extensions"},
		{name: "missing tls13 key share", profile: TLSFingerprintProfile{Name: "test", SupportedVersions: []uint16{0x0304}, Extensions: []uint16{43, 13}}, field: "extensions"},
		{name: "default tls13 missing key share", profile: TLSFingerprintProfile{Name: "test", Extensions: []uint16{43, 13}}, field: "extensions"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.profile.Validate()
			require.Error(t, err)
			validationErr, ok := err.(*ValidationError)
			require.True(t, ok)
			require.Equal(t, test.field, validationErr.Field)
		})
	}
}
