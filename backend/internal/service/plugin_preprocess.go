package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	pluginv2 "github.com/TokenFlux/TokenRouter/pkg/pluginapi/v2"
)

// PluginPreprocessError is a terminal host policy decision, never an upstream
// authentication/network failure and never a reason to replay on another account.
type PluginPreprocessError struct{ Denied bool }

func (e *PluginPreprocessError) Error() string {
	if e.Denied {
		return "Request rejected by extension policy"
	}
	return "Request extension temporarily unavailable"
}

func (m *PluginManager) preprocessRoute(ctx context.Context, account *Account) *extensionRoute {
	if m == nil || account == nil {
		return nil
	}
	table := m.extensions.Load()
	if table == nil {
		return nil
	}
	principal := pluginPrincipalFromContext(ctx)
	for _, route := range table.routes {
		b := route.binding
		if b.Enabled && b.Capability == pluginv2.CapabilityRequestPreprocess && b.Platform == account.Platform &&
			b.AccountType == account.Type && b.RolloutPercent > 0 && int(stablePluginBucket(account.ID)) < b.RolloutPercent &&
			pluginScopeContains(b.AccountIDs, account.ID) && pluginScopeContains(b.UserIDs, principal.userID) && pluginScopeContains(b.GroupIDs, principal.groupID) {
			return route
		}
	}
	if table.stateUnavailable && account.Platform == PlatformOpenAI && (account.Type == AccountTypeOAuth || account.Type == AccountTypeAPIKey) {
		return &extensionRoute{capability: PluginCapability{ID: pluginv2.CapabilityRequestPreprocess, FailureMode: pluginv2.FailureModeClosed},
			unavailable: "插件启用状态暂时无法读取", calls: &extensionCallState{}}
	}
	return nil
}
func (m *PluginManager) ShouldPreprocess(account *Account) bool {
	return m.ShouldPreprocessForRequest(context.Background(), account)
}
func (m *PluginManager) ShouldPreprocessForRequest(ctx context.Context, account *Account) bool {
	return m.preprocessRoute(ctx, account) != nil
}

// PreprocessOpenAI dispatches the first hook at the prepared HTTP request boundary.
// It never gives the plugin credentials, proxy data, or arbitrary inbound headers.
func (m *PluginManager) PreprocessOpenAI(ctx context.Context, request *http.Request, account *Account) (*http.Request, error) {
	route := m.preprocessRoute(ctx, account)
	if route == nil || request == nil || request.URL == nil || request.Method != http.MethodPost {
		return request, nil
	}
	path := strings.TrimSuffix(request.URL.Path, "/")
	if !strings.HasSuffix(path, "/responses") && !strings.HasSuffix(path, "/chat/completions") && !strings.HasSuffix(path, "/responses/compact") {
		return request, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	failed := func() (*http.Request, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if route.capability.FailureMode == pluginv2.FailureModeOpen {
			return request, nil
		}
		return nil, &PluginPreprocessError{}
	}
	if route.runtime == nil || route.runtime.extension == nil ||
		(route.runtime.client != nil && route.runtime.client.Exited()) {
		return failed()
	}
	if err := route.calls.acquire(route.binding.EffectiveConcurrency()); err != nil {
		return failed()
	}
	callFailed := true
	defer func() {
		if ctx.Err() != nil {
			// Client cancellation is not evidence of an unhealthy plugin.
			route.calls.inFlight.Add(-1)
			return
		}
		route.calls.finish(callFailed)
	}()
	if !route.runtime.beginRequest() {
		return failed()
	}
	defer route.runtime.finishRequest()
	callCtx, cancel := context.WithTimeout(ctx, route.binding.Timeout(route.capability))
	defer cancel()
	deadline, _ := callCtx.Deadline()
	requestID := make([]byte, 16)
	if _, err := rand.Read(requestID); err != nil {
		return failed()
	}
	input := pluginv2.PreprocessRequest{Capability: route.capability.ID, Context: pluginv2.RequestContext{
		RequestID: hex.EncodeToString(requestID), Deadline: deadline, Platform: account.Platform, AccountType: account.Type,
		AccountID: account.ID, Method: request.Method, Path: request.URL.Path, Host: request.URL.Hostname(),
		Headers: map[string][]string{},
	}}
	input.Context.TraceID = input.Context.RequestID
	if trace, ok := ctx.Value(ctxkey.RequestID).(string); ok && validPluginTraceID(trace) {
		input.Context.TraceID = trace
	}
	principal := pluginPrincipalFromContext(ctx)
	input.Context.UserID, input.Context.GroupID = principal.userID, principal.groupID
	// Only host-approved metadata crosses the process boundary.
	for _, name := range []string{"content-type", "accept"} {
		if value := request.Header.Get(name); len(value) > 0 && len(value) <= 1024 {
			input.Context.Headers[name] = []string{value}
		}
	}
	var originalBody []byte
	if hasExtensionPermission(route.capability, pluginv2.PermissionRequestBody) {
		if request.GetBody == nil {
			return failed()
		}
		body, err := request.GetBody()
		if err != nil {
			return failed()
		}
		originalBody, err = io.ReadAll(io.LimitReader(body, pluginv2.MaxRequestBodyBytes+1))
		_ = body.Close()
		if err != nil || len(originalBody) > pluginv2.MaxRequestBodyBytes {
			return failed()
		}
		input.BodyJSON = originalBody
		var metadata struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(originalBody, &metadata) != nil {
			return failed()
		}
		input.Context.Model = metadata.Model
	}
	started := time.Now()
	if err := input.Validate(); err != nil {
		return failed()
	}
	decision, err := route.runtime.extension.Preprocess(callCtx, input)
	if err == nil {
		err = callCtx.Err()
	}
	if err == nil {
		err = decision.Validate()
	}
	outcome := "error"
	defer func() {
		slog.Info("plugin_preprocess", "plugin_id", route.pluginID, "capability", route.capability.ID,
			"request_id", input.Context.RequestID, "trace_id", input.Context.TraceID, "decision", outcome, "duration_ms", time.Since(started).Milliseconds())
	}()
	if err != nil {
		return failed()
	}
	switch decision.Decision {
	case pluginv2.DecisionPass:
		callFailed, outcome = false, "pass"
		return request, nil
	case pluginv2.DecisionDeny:
		callFailed, outcome = false, "deny"
		route.calls.denied.Add(1)
		return nil, &PluginPreprocessError{Denied: true}
	case pluginv2.DecisionModify:
		modified, err := applyExtensionPatch(request, originalBody, decision.Patch, route.capability)
		if err != nil {
			return failed()
		}
		callFailed, outcome = false, "modify"
		return modified, nil
	default:
		return failed()
	}
}

func validPluginTraceID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, char := range id {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func hasExtensionPermission(capability PluginCapability, permission pluginv2.Permission) bool {
	for _, candidate := range capability.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}

func applyExtensionPatch(request *http.Request, original []byte, patch *pluginv2.RequestPatch, capability PluginCapability) (*http.Request, error) {
	if !hasExtensionPermission(capability, pluginv2.PermissionRequestMutate) {
		return nil, errors.New("未授权请求修改")
	}
	if patch == nil {
		return nil, errors.New("缺少请求修改")
	}
	if err := patch.Validate(); err != nil {
		return nil, err
	}
	if (patch.Method != "" && patch.Method != request.Method) || (patch.Path != "" && patch.Path != request.URL.Path) {
		return nil, errors.New("请求预处理不能修改已选定的端点和方法")
	}
	for name, values := range patch.Headers {
		if !strings.HasPrefix(name, "x-sub2api-extension-") {
			return nil, errors.New("请求头不在修改白名单内")
		}
		for _, value := range values {
			if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
				return nil, errors.New("请求头值无效")
			}
		}
	}
	if patch.BodyChanged {
		if !hasExtensionPermission(capability, pluginv2.PermissionRequestBody) {
			return nil, errors.New("未授权请求体访问")
		}
		if err := validateExtensionBodyPatch(original, patch.BodyJSON); err != nil {
			return nil, err
		}
	}
	out := request.Clone(request.Context())
	for name, values := range patch.Headers {
		out.Header.Del(name)
		for _, value := range values {
			out.Header.Add(name, value)
		}
	}
	if patch.BodyChanged {
		body := append([]byte(nil), patch.BodyJSON...)
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		out.ContentLength = int64(len(body))
		out.Header.Del("Content-Length")
		out.TransferEncoding = nil
		if request.Body != nil {
			_ = request.Body.Close()
		}
	}
	return out, nil
}

func validateExtensionBodyPatch(before, after []byte) error {
	decode := func(body []byte) (map[string]any, error) {
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if value == nil {
			return nil, errors.New("请求体必须是 JSON 对象")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.New("请求体只能包含一个 JSON 对象")
		}
		return value, nil
	}
	old, err := decode(before)
	if err != nil {
		return err
	}
	next, err := decode(after)
	if err != nil {
		return err
	}
	// This hook runs after account selection. Routing, model, streaming and
	// billable service tier must remain authoritative in the core.
	allowed := map[string]bool{"temperature": true, "top_p": true, "max_tokens": true, "max_output_tokens": true,
		"presence_penalty": true, "frequency_penalty": true, "seed": true, "stop": true, "instructions": true}
	for key, value := range old {
		nextValue, exists := next[key]
		if !allowed[key] && (!exists || !reflect.DeepEqual(value, nextValue)) {
			return fmt.Errorf("插件不能修改核心字段 %s", key)
		}
	}
	for key, value := range next {
		oldValue, exists := old[key]
		if !allowed[key] && (!exists || !reflect.DeepEqual(value, oldValue)) {
			return fmt.Errorf("插件不能修改核心字段 %s", key)
		}
	}
	for _, key := range []string{"temperature", "top_p", "max_tokens", "max_output_tokens", "presence_penalty", "frequency_penalty", "seed"} {
		if reflect.DeepEqual(old[key], next[key]) {
			continue
		}
		if value, exists := next[key]; exists && value != nil {
			number, ok := value.(json.Number)
			if !ok {
				return fmt.Errorf("%s 必须是数值", key)
			}
			n, err := number.Float64()
			if err != nil {
				return err
			}
			switch key {
			case "temperature":
				if n < 0 || n > 2 {
					return errors.New("temperature 超出范围")
				}
			case "top_p":
				if n < 0 || n > 1 {
					return errors.New("top_p 超出范围")
				}
			case "presence_penalty", "frequency_penalty":
				if n < -2 || n > 2 {
					return errors.New("penalty 超出范围")
				}
			case "max_tokens", "max_output_tokens":
				if v, err := number.Int64(); err != nil || v < 1 {
					return errors.New("token 上限必须是正整数")
				}
			case "seed":
				if _, err := number.Int64(); err != nil {
					return errors.New("seed 必须是整数")
				}
			}
		}
	}
	if !reflect.DeepEqual(old["instructions"], next["instructions"]) && next["instructions"] != nil {
		if _, ok := next["instructions"].(string); !ok {
			return errors.New("instructions 必须是字符串")
		}
	}
	if !reflect.DeepEqual(old["stop"], next["stop"]) && next["stop"] != nil {
		switch value := next["stop"].(type) {
		case string:
		case []any:
			if len(value) > 4 {
				return errors.New("stop 最多四项")
			}
			for _, entry := range value {
				if _, ok := entry.(string); !ok {
					return errors.New("stop 必须包含字符串")
				}
			}
		default:
			return errors.New("stop 必须是字符串或字符串数组")
		}
	}
	return nil
}
