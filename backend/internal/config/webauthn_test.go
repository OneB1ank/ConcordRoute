package config

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestValidateWebAuthnConfig(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*Config)
		wantError string
	}{
		{
			name: "valid production origin",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute",
					RPID:          "concordroute.example.com",
					RPOrigins:     []string{"https://concordroute.example.com"},
				}
			},
		},
		{
			name: "valid localhost development origin",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute Dev",
					RPID:          "localhost",
					RPOrigins:     []string{"http://localhost:5173"},
				}
			},
		},
		{
			name: "missing relying party id",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute",
					RPOrigins:     []string{"https://concordroute.example.com"},
				}
			},
			wantError: "webauthn.rp_id",
		},
		{
			name: "relying party id contains scheme",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute",
					RPID:          "https://concordroute.example.com",
					RPOrigins:     []string{"https://concordroute.example.com"},
				}
			},
			wantError: "domain without scheme",
		},
		{
			name: "non-local insecure origin",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute",
					RPID:          "concordroute.example.com",
					RPOrigins:     []string{"http://concordroute.example.com"},
				}
			},
			wantError: "must use HTTPS",
		},
		{
			name: "origin outside relying party id",
			configure: func(cfg *Config) {
				cfg.WebAuthn = WebAuthnConfig{
					Enabled:       true,
					RPDisplayName: "ConcordRoute",
					RPID:          "example.com",
					RPOrigins:     []string{"https://example.net"},
				}
			},
			wantError: "not within relying party ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
			cfg, err := Load()
			require.NoError(t, err)
			tt.configure(cfg)

			err = cfg.Validate()
			if tt.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantError)
			}
		})
	}
}
