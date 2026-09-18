package pluginv2

import (
	"testing"
	"time"
)

func TestCapabilityValidationAndNormalization(t *testing.T) {
	capabilities, err := NormalizeCapabilities([]Capability{
		{
			ID:          CapabilityRequestPreprocess,
			Kind:        CapabilityKindHook,
			Permissions: []Permission{PermissionRequestMutate, PermissionRequestMetadata},
			TimeoutMS:   2000,
			FailureMode: FailureModeClosed,
			Synchronous: true,
		},
	})
	if err != nil {
		t.Fatalf("规范化能力失败: %v", err)
	}
	if got := capabilities[0].Permissions[0]; got != PermissionRequestMetadata {
		t.Fatalf("权限未排序: %q", got)
	}
}

func TestRequestContextRejectsSensitiveHeaders(t *testing.T) {
	request := PreprocessRequest{
		Capability: CapabilityRequestPreprocess,
		Context: RequestContext{
			RequestID:   "req-1",
			Deadline:    time.Now().Add(time.Second),
			Platform:    "openai",
			AccountType: "oauth",
			Method:      "POST",
			Path:        "/v1/responses",
			Headers:     map[string][]string{"authorization": {"secret"}},
		},
	}
	if err := request.Validate(); err == nil {
		t.Fatal("包含 authorization 的请求上下文应该被拒绝")
	}
}

func TestPreprocessResponseDecisions(t *testing.T) {
	valid := PreprocessResponse{
		Decision: DecisionModify,
		Patch: &RequestPatch{
			BodyJSON:    []byte(`{"model":"gpt-5"}`),
			BodyChanged: true,
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("有效修改决策被拒绝: %v", err)
	}

	invalid := PreprocessResponse{Decision: DecisionDeny}
	if err := invalid.Validate(); err == nil {
		t.Fatal("没有原因的 deny 决策应该被拒绝")
	}
}

func TestPluginInfoRejectsDuplicateCapabilities(t *testing.T) {
	capability := Capability{
		ID:          CapabilityRequestPreprocess,
		Kind:        CapabilityKindHook,
		TimeoutMS:   1000,
		FailureMode: FailureModeClosed,
		Synchronous: true,
	}
	info := PluginInfo{
		PluginID:        "example.plugin",
		PluginVersion:   "1.0.0",
		ProtocolVersion: ProtocolVersion,
		Capabilities:    []Capability{capability, capability},
	}
	if err := info.Validate(); err == nil {
		t.Fatal("重复能力应该被拒绝")
	}
}

func TestNormalizeCapabilitiesDoesNotMutateCallerPermissions(t *testing.T) {
	input := []Capability{{ID: CapabilityRequestPreprocess, Kind: CapabilityKindHook, TimeoutMS: 100, FailureMode: FailureModeClosed,
		Synchronous: true, Permissions: []Permission{PermissionRequestMutate, PermissionRequestMetadata}}}
	if _, err := NormalizeCapabilities(input); err != nil {
		t.Fatal(err)
	}
	if input[0].Permissions[0] != PermissionRequestMutate {
		t.Fatal("normalization mutated the caller's permission slice")
	}
}

func TestPatchRejectsHeaderInjectionAndUnmarkedBody(t *testing.T) {
	for _, patch := range []RequestPatch{
		{Headers: map[string][]string{"x-test": {"ok\r\nAuthorization: injected"}}},
		{Headers: map[string][]string{"x-api-key": {"secret"}}},
		{Headers: map[string][]string{" x-test ": {"invalid"}}},
		{Headers: map[string][]string{"x-test": {"valid"}}, BodyJSON: []byte(`{}`)},
	} {
		if err := patch.Validate(); err == nil {
			t.Fatalf("invalid patch accepted: %#v", patch)
		}
	}
}
