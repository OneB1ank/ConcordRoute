package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
)

const pluginSecretMaxBytes = 16 * 1024

var pluginSecretAliasPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type pluginSecretEnvelope struct {
	Purpose          string `json:"purpose"`
	PluginID         int64  `json:"plugin_id"`
	Capability       string `json:"capability"`
	Alias            string `json:"alias"`
	Value            string `json:"value"`
	ExpiresUnixMicro int64  `json:"expires_unix_micro"`
}

type PluginSecretGrant struct {
	PluginID       int64     `json:"plugin_id"`
	Capability     string    `json:"capability"`
	Alias          string    `json:"alias"`
	ExpiresAt      time.Time `json:"expires_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	EncryptedValue string    `json:"-"`
}
type PluginSecretRepository interface {
	PruneSecretGrants(context.Context) error
	ListSecretGrants(context.Context, int64) ([]PluginSecretGrant, error)
	PutSecretGrant(context.Context, PluginSecretGrant) error
	GetSecretGrant(context.Context, int64, string, string) (*PluginSecretGrant, error)
	DeleteSecretGrant(context.Context, int64, string, string) error
}

func (m *PluginManager) ListSecretGrants(ctx context.Context, id int64) ([]PluginSecretGrant, error) {
	repo, ok := m.repo.(PluginSecretRepository)
	if !ok {
		return nil, errors.New("秘密授权存储不可用")
	}
	return repo.ListSecretGrants(ctx, id)
}
func (m *PluginManager) PutSecretGrant(ctx context.Context, id int64, capability, alias, value string, ttlSeconds int) error {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	repo, ok := m.repo.(PluginSecretRepository)
	if !ok {
		return errors.New("秘密授权存储不可用")
	}
	if !pluginSecretAliasPattern.MatchString(alias) || len(value) == 0 || len(value) > pluginSecretMaxBytes || ttlSeconds < 1 || ttlSeconds > 3600 {
		return errors.New("秘密别名、值或有效期无效（最长一小时）")
	}
	i, err := m.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	allowed := false
	for _, cap := range i.Manifest.Capabilities {
		if cap.ID == capability && hasExtensionPermission(cap, pluginv2.PermissionSecretBroker) {
			allowed = true
		}
	}
	if i.Manifest.SchemaVersion != 2 || !allowed {
		return errors.New("插件能力未声明 secrets.broker 权限")
	}
	if m.encryptor == nil {
		return errors.New("秘密加密服务不可用")
	}
	expires := time.Now().UTC().Truncate(time.Microsecond).Add(time.Duration(ttlSeconds) * time.Second)
	envelope, _ := json.Marshal(pluginSecretEnvelope{Purpose: "sub2api.plugin-secret.v1", PluginID: id, Capability: capability, Alias: alias, Value: value, ExpiresUnixMicro: expires.UnixMicro()})
	cipher, err := m.encryptor.Encrypt(string(envelope))
	if err != nil {
		return errors.New("加密秘密失败")
	}
	return repo.PutSecretGrant(ctx, PluginSecretGrant{PluginID: id, Capability: capability, Alias: alias, EncryptedValue: cipher, ExpiresAt: expires})
}
func (m *PluginManager) DeleteSecretGrant(ctx context.Context, id int64, capability, alias string) error {
	repo, ok := m.repo.(PluginSecretRepository)
	if !ok {
		return errors.New("秘密授权存储不可用")
	}
	return repo.DeleteSecretGrant(ctx, id, capability, alias)
}
func (m *PluginManager) readPluginSecret(ctx context.Context, id int64, capability, alias string) (pluginv2.SecretValue, error) {
	repo, ok := m.repo.(PluginSecretRepository)
	if !ok || m.encryptor == nil {
		return pluginv2.SecretValue{}, errors.New("secret unavailable")
	}
	grant, err := repo.GetSecretGrant(ctx, id, capability, alias)
	if err != nil || grant == nil || !time.Now().Before(grant.ExpiresAt) {
		return pluginv2.SecretValue{}, errors.New("secret unavailable")
	}
	value, err := m.encryptor.Decrypt(grant.EncryptedValue)
	if err != nil {
		return pluginv2.SecretValue{}, errors.New("secret unavailable")
	}
	var envelope pluginSecretEnvelope
	if json.Unmarshal([]byte(value), &envelope) != nil || envelope.Purpose != "sub2api.plugin-secret.v1" || envelope.PluginID != id ||
		envelope.Capability != capability || envelope.Alias != alias || envelope.ExpiresUnixMicro != grant.ExpiresAt.UnixMicro() {
		return pluginv2.SecretValue{}, errors.New("secret unavailable")
	}
	return pluginv2.SecretValue{Value: []byte(envelope.Value), ExpiresAt: grant.ExpiresAt}, nil
}
