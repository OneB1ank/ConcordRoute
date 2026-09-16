package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
)

const liveAttestationHeader = "x-oai-attestation"

type liveAttestationAES struct {
	key [32]byte
}

func newLiveAttestationCipher(cfg *config.Config) SecretEncryptor {
	if cfg == nil || strings.TrimSpace(cfg.JWT.Secret) == "" {
		return nil
	}
	return &liveAttestationAES{
		// 加密域属于持久化格式，保留旧值才能解密升级前保存的证明数据。
		key: sha256.Sum256([]byte("tokenrouter/live-attestation/v1\x00" + cfg.JWT.Secret)),
	}
}

func (c *liveAttestationAES) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate Live attestation nonce: %w", err)
	}
	encrypted := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(encrypted), nil
}

func (c *liveAttestationAES) Decrypt(ciphertext string) (string, error) {
	encrypted, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode Live attestation: %w", err)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(encrypted) < gcm.NonceSize() {
		return "", errors.New("encrypted Live attestation is too short")
	}
	plaintext, err := gcm.Open(nil, encrypted[:gcm.NonceSize()], encrypted[gcm.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt Live attestation: %w", err)
	}
	return string(plaintext), nil
}

func (s *OpenAIGatewayService) prepareLiveAttestation(ctx context.Context) (string, string, error) {
	if s == nil {
		return "", "", &LiveAttestationUnavailableError{
			Reason: "Live service is unavailable",
		}
	}
	// 没有提供器表示没有可附带的证明，而不是 Live 传输不可用。
	// 上游决定账号是否需要证明；不合成成功状态，也不改变认证或账号准入。
	if s.liveAttestation == nil {
		return "", "", nil
	}
	header, err := s.liveAttestation.Generate(ctx)
	if err != nil {
		if errors.Is(err, liveattestation.ErrLinuxProviderMissing) ||
			errors.Is(err, liveattestation.ErrUnsupportedPlatform) ||
			errors.Is(err, liveattestation.ErrChatGPTAppMissing) {
			return "", "", nil
		}
		return "", "", &LiveAttestationUnavailableError{Reason: err.Error()}
	}
	if strings.TrimSpace(header) == "" {
		return "", "", &LiveAttestationUnavailableError{Reason: "configured provider returned an empty attestation"}
	}
	if s.liveAttestationCipher == nil {
		return "", "", &LiveAttestationUnavailableError{
			Reason: "JWT secret is required to protect the Sideband attestation",
		}
	}
	ciphertext, err := s.liveAttestationCipher.Encrypt(header)
	if err != nil {
		return "", "", &LiveAttestationUnavailableError{
			Reason: "failed to protect the generated DeviceCheck attestation",
		}
	}
	return header, ciphertext, nil
}

// prepareLiveAttestationForRequest 优先复用已经完成 app-server 协商的证明；
// 没有内部 context 时，仅接受官方客户端已经生成的 envelope。Linux 网关
// 不生成 Windows 客户端证明，缺少客户端 envelope 时才回退到已配置的平台
// provider（例如 macOS DeviceCheck 或 Linux 外部 helper）；也没有提供器时
// 省略证明，让真实上游校验账号资格。已提供的异常证明不静默丢弃。
// @project-doc docs/interfaces/openai_upstream.md#openai_live_runtime
func (s *OpenAIGatewayService) prepareLiveAttestationForRequest(
	ctx context.Context,
	account *Account,
	identity LiveCallIdentity,
) (string, string, error) {
	if accountSupportsCodexAppServerAttestation(account) {
		// app-server 已完成能力协商时，证明存储按账号/连接/session/thread
		// 隔离；这里仅复用已校验的 opaque envelope。
		if value, ok := s.resolveCodexClientAttestation(ctx, account); ok {
			if s.liveAttestationCipher == nil {
				return "", "", &LiveAttestationUnavailableError{Reason: "JWT secret is required to protect the Sideband attestation"}
			}
			ciphertext, err := s.liveAttestationCipher.Encrypt(value)
			if err != nil {
				return "", "", &LiveAttestationUnavailableError{Reason: "failed to protect the negotiated client attestation"}
			}
			return value, ciphertext, nil
		}
		if value, ok := normalizeTrustedCodexClientAttestation(codexClientAttestationCandidate{
			Envelope:   identity.ClientAttestationEnvelope,
			UserAgent:  identity.UserAgent,
			Originator: identity.Originator,
		}); ok {
			if s.liveAttestationCipher == nil {
				return "", "", &LiveAttestationUnavailableError{Reason: "JWT secret is required to protect the client attestation"}
			}
			ciphertext, err := s.liveAttestationCipher.Encrypt(value)
			if err != nil {
				return "", "", &LiveAttestationUnavailableError{Reason: "failed to protect the client attestation"}
			}
			return value, ciphertext, nil
		}
		if strings.TrimSpace(identity.ClientAttestationEnvelope) != "" {
			return "", "", &LiveAttestationUnavailableError{Reason: "client attestation envelope is invalid or its client identity is untrusted"}
		}
	}
	return s.prepareLiveAttestation(ctx)
}

func (s *OpenAIGatewayService) decryptLiveAttestation(record *LiveCallRecord) (string, error) {
	if record == nil {
		return "", ErrLiveCallNotFound
	}
	// 创建时未携带证明的会话，Sideband 同样省略；非空密文仍严格解密。
	if record.AttestationCiphertext == "" {
		return "", nil
	}
	if s.liveAttestationCipher == nil {
		return "", &LiveAttestationUnavailableError{
			Reason: "the Live call has no reusable DeviceCheck attestation",
		}
	}
	header, err := s.liveAttestationCipher.Decrypt(record.AttestationCiphertext)
	if err != nil {
		return "", &LiveAttestationUnavailableError{
			Reason: "the Live call DeviceCheck attestation cannot be decrypted on this instance",
		}
	}
	return header, nil
}
