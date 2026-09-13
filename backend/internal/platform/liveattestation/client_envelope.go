package liveattestation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	maxClientAttestationTokenBytes  = 2048
	maxClientAttestationHeaderBytes = 4096
)

// ClientEnvelope is the app-server attestation envelope sent upstream.
// Token remains opaque and is never generated or transformed by the server.
type ClientEnvelope struct {
	Version int    `json:"v"`
	Status  int    `json:"s"`
	Token   string `json:"t,omitempty"`
}

// NormalizeClientEnvelope validates and canonicalizes a client-owned
// attestation envelope. It is intentionally syntax-only; authenticity remains
// the responsibility of the upstream verifier.
func NormalizeClientEnvelope(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty attestation envelope")
	}
	if len(raw) > maxClientAttestationHeaderBytes {
		return "", errors.New("attestation envelope exceeds size limit")
	}
	var envelope ClientEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return "", fmt.Errorf("decode attestation envelope: %w", err)
	}
	if envelope.Version != 1 {
		return "", errors.New("unsupported attestation envelope version")
	}
	if envelope.Status < 0 || envelope.Status > 4 {
		return "", errors.New("invalid attestation envelope status")
	}
	if envelope.Status == 0 {
		if envelope.Token == "" {
			return "", errors.New("successful attestation requires a non-empty token")
		}
		if len(envelope.Token) > maxClientAttestationTokenBytes {
			return "", errors.New("attestation token exceeds size limit")
		}
	} else if envelope.Token != "" {
		return "", errors.New("failed attestation envelope must omit token")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode attestation envelope: %w", err)
	}
	return string(encoded), nil
}
