package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
	"github.com/tidwall/gjson"
)

type PluginConfigCASRepository interface {
	UpdateConfigCAS(context.Context, int64, string, string, string) error
}

func hasProtectionCapability(manifest PluginManifest) bool {
	for _, cap := range manifest.Capabilities {
		if cap.ID == pluginv2.CapabilityProtectionTransport {
			return true
		}
	}
	return false
}

// restoreStoredProtectionConfig replays a previously persisted, already
// validated snapshot. It must bypass revision normalization: the failed
// candidate may have advanced the child process's in-memory revision, while
// the database snapshot being restored is intentionally older.
func restoreStoredProtectionConfig(ctx context.Context, runtime *pluginRuntime, raw []byte) error {
	result, err := runtime.applyConfig(ctx, raw)
	if err != nil {
		return err
	}
	if result == nil || !result.Applied {
		return errors.New("恢复保护配置失败")
	}
	if runtime.host != nil {
		runtime.host.setConfig(raw)
	}
	return nil
}

func validateProtectionTransportCapability(c PluginCapability) error {
	if err := c.ExtensionCapability().Validate(); err != nil {
		return err
	}
	if c.Kind != pluginv2.CapabilityKindProvider || !c.Synchronous || c.FailureMode != pluginv2.FailureModeClosed ||
		c.Platform != PlatformOpenAI || c.AccountType != AccountTypeOAuth || c.TimeoutMS > 120000 {
		return errors.New("保护传输必须是 openai/oauth 同步 provider，并使用 fail_closed")
	}
	required := map[pluginv2.Permission]bool{
		pluginv2.PermissionRequestMetadata: false, pluginv2.PermissionRequestBody: false,
		pluginv2.PermissionCredentialsForward: false, pluginv2.PermissionNetworkOutbound: false,
		pluginv2.PermissionAccountProtection: false, pluginv2.PermissionOriginalRequest: false,
	}
	for _, permission := range c.Permissions {
		if _, ok := required[permission]; ok {
			required[permission] = true
			continue
		}
		switch permission {
		case pluginv2.PermissionHostLog, pluginv2.PermissionHostMetric, pluginv2.PermissionHostConfig, pluginv2.PermissionEventPublish:
		default:
			return errors.New("保护传输权限不受支持")
		}
	}
	for _, granted := range required {
		if !granted {
			return errors.New("保护传输缺少必需权限")
		}
	}
	return nil
}

type pluginProtectionOriginalKey struct{}
type pluginProtectionOriginal struct {
	accountID   int64
	body        []byte
	mappedModel string
}

func withPluginProtectionOriginal(ctx context.Context, account *Account, body []byte) context.Context {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return ctx
	}
	snapshot := pluginProtectionOriginal{accountID: account.ID}
	if len(body) <= pluginv2.MaxRequestBodyBytes {
		snapshot.body = bytes.Clone(body)
		snapshot.mappedModel = normalizeOpenAIModelForUpstream(account, account.GetMappedModel(gjson.GetBytes(body, "model").String()))
	}
	return context.WithValue(ctx, pluginProtectionOriginalKey{}, snapshot)
}
func (r *pluginRuntime) protectionOriginalBody(ctx context.Context, account *Account) []byte {
	if r.transport == nil {
		return nil
	}
	snapshot, _ := ctx.Value(pluginProtectionOriginalKey{}).(pluginProtectionOriginal)
	if snapshot.accountID != account.ID {
		return nil
	}
	return snapshot.body
}
func (r *pluginRuntime) protectionAccountMetadata(ctx context.Context, account *Account) []byte {
	if r.transport == nil {
		return nil
	}
	extra := map[string]any{}
	// Account credentials, model mappings, arbitrary extra fields and secrets never cross here.
	for _, key := range []string{"codex_fingerprint_mode", "codex_fingerprint_seed", "openai_device_id", "enable_tls_fingerprint", "tls_fingerprint_builtin", "request_integrity_mode"} {
		if value, ok := account.Extra[key]; ok {
			switch value := value.(type) {
			case string:
				if len(value) <= 256 {
					extra[key] = value
				}
			case bool:
				extra[key] = value
			}
		}
	}
	snapshot, _ := ctx.Value(pluginProtectionOriginalKey{}).(pluginProtectionOriginal)
	mappedModel := ""
	if deviceID := account.GetOpenAIDeviceID(); deviceID != "" && len(deviceID) <= 256 {
		extra["openai_device_id"] = deviceID
	}
	if snapshot.accountID == account.ID {
		mappedModel = snapshot.mappedModel
	}
	digest := sha256.Sum256([]byte(defaultCodexSynthInstructions(mappedModel)))
	raw, _ := json.Marshal(pluginv2.ProtectionAccount{ID: account.ID, Platform: account.Platform, Type: account.Type,
		Concurrency: account.Concurrency, Shadow: account.IsShadow(), Extra: extra, MappedModel: mappedModel, DefaultInstructionsDigest: hex.EncodeToString(digest[:])})
	if len(raw) > 64*1024 {
		return nil
	}
	return raw
}
func (m *PluginManager) protectionTransportRoute(ctx context.Context, account *Account, scope bool) *extensionRoute {
	if m == nil || account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth {
		return nil
	}
	table := m.extensions.Load()
	if table == nil {
		return nil
	}
	principal := pluginPrincipalFromContext(ctx)
	for _, route := range table.routes {
		b := route.binding
		if b.Enabled && b.Capability == pluginv2.CapabilityProtectionTransport && b.Platform == account.Platform &&
			b.AccountType == account.Type && b.RolloutPercent > int(stablePluginBucket(account.ID)) && pluginScopeContains(b.AccountIDs, account.ID) &&
			(!scope || (pluginScopeContains(b.UserIDs, principal.userID) && pluginScopeContains(b.GroupIDs, principal.groupID))) {
			return route
		}
	}
	return nil
}
func (m *PluginManager) hasProtectionTransport(account *Account) bool {
	return m.protectionTransportRoute(context.Background(), account, false) != nil
}
func (m *PluginManager) roundTripProtection(ctx context.Context, req *http.Request, proxyURL string, account *Account) (*http.Response, bool, error) {
	route := m.protectionTransportRoute(ctx, account, true)
	if route == nil {
		return nil, false, nil
	}
	fail := func() (*http.Response, bool, error) { return nil, true, &PluginPreprocessError{} }
	rt := route.runtime
	if rt == nil || rt.transport == nil || (rt.client != nil && rt.client.Exited()) {
		return fail()
	}
	if err := route.calls.acquire(route.binding.EffectiveConcurrency()); err != nil {
		return fail()
	}
	if !rt.beginRequest() {
		route.calls.finish(true)
		return fail()
	}
	callCtx, cancel := context.WithCancel(ctx)
	headerTimer := time.AfterFunc(route.binding.Timeout(route.capability), cancel)
	response, err := rt.roundTrip(callCtx, req, proxyURL, account)
	headerTimer.Stop()
	if err != nil {
		cancel()
		rt.finishRequest()
		// A local policy rejection is never treated as an upstream authentication failure.
		var transportErr *PluginTransportError
		if errors.As(err, &transportErr) && (transportErr.Code == "PROTECTION_DENIED" || transportErr.Code == "PROTECTION_BUSY") {
			route.calls.finish(false)
			if transportErr.Code == "PROTECTION_DENIED" {
				route.calls.denied.Add(1)
			}
			return nil, true, &PluginPreprocessError{Denied: transportErr.Code == "PROTECTION_DENIED"}
		}
		route.calls.finish(ctx.Err() == nil)
		return nil, true, err
	}
	response.Body = &protectionResponseBody{ReadCloser: response.Body, done: func(failed bool) { route.calls.finish(failed && ctx.Err() == nil) }}
	response.Body = &protectionCancelBody{ReadCloser: response.Body, cancel: cancel}
	return response, true, nil
}

type protectionCancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *protectionCancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

type protectionResponseBody struct {
	io.ReadCloser
	once sync.Once
	done func(bool)
}

func (b *protectionResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(func() { b.done(err != io.EOF) })
	}
	return n, err
}
func (b *protectionResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.done(err != nil) })
	return err
}
