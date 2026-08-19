package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/xai"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCreateAccountDiscardsDeprecatedBillingProbeExtra(t *testing.T) {
	repo := &accountServiceTestRepo{}
	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "upstream",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind: true,
		Extra: map[string]any{
			deprecatedUpstreamBillingProbeEnabledExtraKey: true,
			deprecatedUpstreamBillingProbeExtraKey:        map[string]any{"status": "ok"},
			"custom":                                      "value",
		},
	})

	require.NoError(t, err)
	require.NotContains(t, created.Extra, deprecatedUpstreamBillingProbeEnabledExtraKey)
	require.NotContains(t, created.Extra, deprecatedUpstreamBillingProbeExtraKey)
	require.Equal(t, "value", created.Extra["custom"])
}

func TestUpdateAccountDiscardsDeprecatedBillingProbeExtra(t *testing.T) {
	accountID := int64(110)
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				deprecatedUpstreamBillingProbeEnabledExtraKey: true,
				deprecatedUpstreamBillingProbeExtraKey:        map[string]any{"status": "ok"},
			},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			deprecatedUpstreamBillingProbeEnabledExtraKey: false,
			deprecatedUpstreamBillingProbeExtraKey:        map[string]any{"status": "forged"},
			"custom":                                      "value",
		},
	})

	require.NoError(t, err)
	require.NotContains(t, updated.Extra, deprecatedUpstreamBillingProbeEnabledExtraKey)
	require.NotContains(t, updated.Extra, deprecatedUpstreamBillingProbeExtraKey)
	require.Equal(t, "value", updated.Extra["custom"])
}

func TestBulkUpdateAccountsDiscardsDeprecatedBillingProbeExtra(t *testing.T) {
	repo := &accountServiceTestRepo{}
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		Extra: map[string]any{
			deprecatedUpstreamBillingProbeEnabledExtraKey: true,
			deprecatedUpstreamBillingProbeExtraKey:        map[string]any{"status": "ok"},
			"custom":                                      "value",
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.NotContains(t, repo.bulkUpdates[0].Extra, deprecatedUpstreamBillingProbeEnabledExtraKey)
	require.NotContains(t, repo.bulkUpdates[0].Extra, deprecatedUpstreamBillingProbeExtraKey)
	require.Equal(t, "value", repo.bulkUpdates[0].Extra["custom"])
}

func TestUpdateAccountPreservesCodexFingerprintSeed(t *testing.T) {
	accountID := int64(113)
	seed := uuid.NewString()
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra: map[string]any{
				CodexFingerprintSeedExtraKey: seed,
			},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			CodexFingerprintSeedExtraKey: "forged-seed",
			"custom":                     "value",
		},
	})

	require.NoError(t, err)
	require.Equal(t, seed, updated.Extra[CodexFingerprintSeedExtraKey])
	require.Equal(t, "value", updated.Extra["custom"])
}

func TestUpdateAccountDiscardsIncomingCodexFingerprintSeedWhenLegacyAccountHasNone(t *testing.T) {
	accountID := int64(114)
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra:    map[string]any{},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			CodexFingerprintSeedExtraKey: "forged-seed",
			"custom":                     "value",
		},
	})

	require.NoError(t, err)
	require.NotContains(t, updated.Extra, CodexFingerprintSeedExtraKey)
	require.Equal(t, "value", updated.Extra["custom"])
}

func TestUpdateAccountDisablingCodexQuotaOverdraftClearsProbeState(t *testing.T) {
	accountID := int64(115)
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra: map[string]any{
				CodexQuotaOverdraftEnabledExtraKey: true,
				CodexQuotaOverdraftProbeExtraKey: map[string]any{
					"status":    codexQuotaOverdraftProbePassed,
					"cycle_key": "5h:1787166000",
				},
			},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			CodexQuotaOverdraftEnabledExtraKey: false,
			"custom":                           "value",
		},
	})

	require.NoError(t, err)
	require.Equal(t, false, updated.Extra[CodexQuotaOverdraftEnabledExtraKey])
	require.NotContains(t, updated.Extra, CodexQuotaOverdraftProbeExtraKey)
	require.Equal(t, "value", updated.Extra["custom"])
}

func TestUpdateAccountEnablingCodexQuotaOverdraftClearsOnlyLegacyThresholdRuntimeBlock(t *testing.T) {
	accountID := int64(116)
	resetAt := time.Now().UTC().Add(time.Hour)
	account := &Account{
		ID:                      accountID,
		Platform:                PlatformOpenAI,
		Type:                    AccountTypeOAuth,
		Status:                  StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &resetAt,
		TempUnschedulableReason: BuildAccountSchedulingThresholdReason("quota exhausted"),
		Extra:                   map[string]any{},
	}
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{accountID: account}}
	gateway := &OpenAIGatewayService{}
	gateway.BlockAccountScheduling(account, resetAt, "account_scheduling_threshold")
	require.True(t, gateway.isOpenAIAccountRuntimeBlocked(account))

	updated, err := (&adminServiceImpl{accountRepo: repo, runtimeBlocker: gateway}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{CodexQuotaOverdraftEnabledExtraKey: true},
	})

	require.NoError(t, err)
	require.True(t, updated.IsCodexQuotaOverdraftEnabled())
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(updated))
}

func TestUpdateAccountEnablingCodexQuotaOverdraftPreservesNewerRuntimeBlock(t *testing.T) {
	accountID := int64(117)
	resetAt := time.Now().UTC().Add(time.Hour)
	account := &Account{
		ID:                      accountID,
		Platform:                PlatformOpenAI,
		Type:                    AccountTypeOAuth,
		Status:                  StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &resetAt,
		TempUnschedulableReason: BuildAccountSchedulingThresholdReason("quota exhausted"),
		Extra:                   map[string]any{},
	}
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{accountID: account}}
	gateway := &OpenAIGatewayService{}
	gateway.BlockAccountScheduling(account, resetAt.Add(time.Hour), "oauth_401")

	updated, err := (&adminServiceImpl{accountRepo: repo, runtimeBlocker: gateway}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{CodexQuotaOverdraftEnabledExtraKey: true},
	})

	require.NoError(t, err)
	require.True(t, gateway.isOpenAIAccountRuntimeBlocked(updated), "开启透支不得清除并发产生的认证阻断")
}

func TestBulkUpdateAccountsDiscardsCodexFingerprintSeed(t *testing.T) {
	repo := &accountServiceTestRepo{}
	result, err := (&adminServiceImpl{accountRepo: repo}).BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{1},
		Extra: map[string]any{
			CodexFingerprintSeedExtraKey: "forged-seed",
			"custom":                     "value",
		},
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.NotContains(t, repo.bulkUpdates[0].Extra, CodexFingerprintSeedExtraKey)
	require.Equal(t, "value", repo.bulkUpdates[0].Extra["custom"])
}

func TestUpdateAccountPreservesGrokBillingSnapshotForUnrelatedEdit(t *testing.T) {
	accountID := int64(112)
	billing := &xai.BillingSummary{
		StatusCode:       http.StatusForbidden,
		WeeklyStatusCode: http.StatusForbidden,
	}
	repo := &accountServiceTestRepo{accounts: map[int64]*Account{
		accountID: {
			ID:       accountID,
			Platform: PlatformGrok,
			Type:     AccountTypeOAuth,
			Status:   StatusActive,
			Extra:    map[string]any{grokBillingExtraKey: billing},
		},
	}}

	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{"custom": "value"},
	})

	require.NoError(t, err)
	require.Equal(t, billing, updated.Extra[grokBillingExtraKey])
	require.Equal(t, "value", updated.Extra["custom"])
	eligible, reason := updated.GrokMediaGenerationEligibility()
	require.False(t, eligible)
	require.Equal(t, "billing_forbidden", reason)
}
