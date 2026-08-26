//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/stretchr/testify/require"
)

func TestShouldMap429To503OnlyForNoResetFallback(t *testing.T) {
	repo := newMockSettingRepo()
	data, err := json.Marshal(RateLimit429CooldownSettings{Enabled: true, CooldownSeconds: 5, MapTo503: true})
	require.NoError(t, err)
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewRateLimitService(&rateLimit429AccountRepoStub{}, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(NewSettingService(repo, &config.Config{}))
	account := &Account{Platform: PlatformOpenAI}
	loaded, err := svc.settingService.GetRateLimit429CooldownSettings(context.Background())
	require.NoError(t, err)
	require.True(t, loaded.Enabled)
	require.True(t, loaded.MapTo503)

	require.True(t, svc.ShouldMap429To503(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"rate limited"}}`)))
	resetHeaders := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"30"},
		"X-Codex-Primary-Window-Minutes":      []string{"10080"},
	}
	require.NotNil(t, ParseCodexRateLimitHeaders(resetHeaders))
	require.NotNil(t, calculateOpenAI429ResetTime(resetHeaders))
	require.False(t, svc.ShouldMap429To503(context.Background(), account, resetHeaders, nil))
}

func TestShouldMap429To503HonorsDisabledMapping(t *testing.T) {
	repo := newMockSettingRepo()
	data, err := json.Marshal(RateLimit429CooldownSettings{Enabled: true, CooldownSeconds: 5, MapTo503: false})
	require.NoError(t, err)
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewRateLimitService(&rateLimit429AccountRepoStub{}, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(NewSettingService(repo, &config.Config{}))
	account := &Account{Platform: PlatformOpenAI}

	require.False(t, svc.ShouldMap429To503(context.Background(), account, http.Header{}, nil))
}

func TestShouldMap429To503KeepsQuotaExhaustionAs429(t *testing.T) {
	repo := newMockSettingRepo()
	data, err := json.Marshal(RateLimit429CooldownSettings{Enabled: true, CooldownSeconds: 5, MapTo503: true})
	require.NoError(t, err)
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewRateLimitService(&rateLimit429AccountRepoStub{}, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(NewSettingService(repo, &config.Config{}))
	account := &Account{Platform: PlatformOpenAI}

	require.False(t, svc.ShouldMap429To503(
		context.Background(), account, http.Header{}, []byte(`{"error":{"type":"usage_limit_reached"}}`)),
		"quota exhaustion must remain 429 for the overdraft state machine")
	require.True(t, svc.ShouldMap429To503(
		context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_exceeded"}}`)),
		"transient 429 without reset may use the configured 503 mapping")
}
