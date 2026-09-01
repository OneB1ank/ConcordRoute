package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveBillingServiceTier(t *testing.T) {
	tests := []struct {
		name       string
		requested  string
		observed   string
		billing    string
		downgraded bool
	}{
		{name: "priority downgraded to default", requested: "priority", observed: "default", billing: "default", downgraded: true},
		{name: "priority downgraded to flex", requested: "priority", observed: "flex", billing: "flex", downgraded: true},
		{name: "matching priority", requested: "priority", observed: "priority", billing: "priority"},
		{name: "response never promotes untiered request", observed: "priority"},
		{name: "unknown response ignored", requested: "priority", observed: "turbo", billing: "priority"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveBillingServiceTier(tt.requested, tt.observed)
			require.Equal(t, tt.billing, got.Billing)
			require.Equal(t, tt.downgraded, got.Downgraded)
		})
	}
}

func TestApplyOpenAIServiceTierBillingResolution(t *testing.T) {
	t.Run("Codex exception only covers OpenAI default", func(t *testing.T) {
		require.True(t, codexOAuthResponseTierIsNonAuthoritative("default"))
		require.False(t, codexOAuthResponseTierIsNonAuthoritative("standard"))
		require.False(t, codexOAuthResponseTierIsNonAuthoritative("flex"))
	})

	t.Run("API Key accepts response downgrade", func(t *testing.T) {
		requested := "priority"
		result := &OpenAIForwardResult{ServiceTier: &requested, UpstreamResponseServiceTier: "default"}
		resolution := ApplyOpenAIServiceTierBillingResolution(
			&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, result,
		)
		require.True(t, resolution.Downgraded)
		require.Equal(t, "default", *result.ServiceTier)
	})

	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run("Codex "+accountType+" keeps outbound priority on default echo", func(t *testing.T) {
			requested := "priority"
			result := &OpenAIForwardResult{ServiceTier: &requested, UpstreamResponseServiceTier: "default"}
			resolution := ApplyOpenAIServiceTierBillingResolution(
				&Account{Platform: PlatformOpenAI, Type: accountType}, result,
			)
			require.False(t, resolution.Downgraded)
			require.Equal(t, "priority", resolution.Billing)
			require.Same(t, &requested, result.ServiceTier)
		})

		t.Run("Codex "+accountType+" accepts explicit flex downgrade", func(t *testing.T) {
			requested := "priority"
			result := &OpenAIForwardResult{ServiceTier: &requested, UpstreamResponseServiceTier: "flex"}
			resolution := ApplyOpenAIServiceTierBillingResolution(
				&Account{Platform: PlatformOpenAI, Type: accountType}, result,
			)
			require.True(t, resolution.Downgraded)
			require.Equal(t, "flex", *result.ServiceTier)
		})
	}

	t.Run("response never promotes untiered request", func(t *testing.T) {
		result := &OpenAIForwardResult{UpstreamResponseServiceTier: "priority"}
		resolution := ApplyOpenAIServiceTierBillingResolution(
			&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, result,
		)
		require.False(t, resolution.Downgraded)
		require.Empty(t, resolution.Billing)
		require.Nil(t, result.ServiceTier)
	})

	t.Run("non OpenAI OAuth uses generic response contract", func(t *testing.T) {
		requested := "priority"
		result := &OpenAIForwardResult{ServiceTier: &requested, UpstreamResponseServiceTier: "default"}
		resolution := ApplyOpenAIServiceTierBillingResolution(
			&Account{Platform: PlatformGrok, Type: AccountTypeOAuth}, result,
		)
		require.True(t, resolution.Downgraded)
		require.Equal(t, "default", *result.ServiceTier)
	})

	require.False(t, ApplyOpenAIServiceTierBillingResolution(nil, nil).Downgraded)
}

func TestObservedOpenAIServiceTierFromPayload(t *testing.T) {
	require.Equal(t, "default", observedOpenAIServiceTierFromPayload([]byte(
		`{"type":"response.completed","response":{"service_tier":"default"}}`,
	)))
	require.Equal(t, "priority", observedOpenAIServiceTierFromPayload([]byte(
		`{"service_tier":"fast"}`,
	)))
	require.Empty(t, observedOpenAIServiceTierFromPayload([]byte(`{"service_tier":"turbo"}`)))
}
