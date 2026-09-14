package liveattestation

import (
	"context"
	"errors"
)

var (
	ErrUnsupportedPlatform  = errors.New("server-side Live attestation requires Apple Silicon macOS or a configured Linux attestation helper")
	ErrChatGPTAppMissing    = errors.New("live attestation requires the official ChatGPT app on the ConcordRoute server")
	ErrLinuxProviderMissing = errors.New("linux Live attestation requires a configured external attestation helper")
	ErrLinuxProviderInvalid = errors.New("linux Live attestation helper is missing or not executable")
)

// Provider 在发起 Live 请求前生成 ChatGPT DeviceCheck attestation。
type Provider interface {
	Check(ctx context.Context) error
	Generate(ctx context.Context) (string, error)
}
