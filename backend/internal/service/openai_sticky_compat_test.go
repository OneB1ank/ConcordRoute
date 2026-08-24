package service

import (
	"context"
	"errors"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

type stickyCompatLookupCache struct {
	*stubGatewayCache
	getErrors map[string]error
	getKeys   []string
}

func (c *stickyCompatLookupCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	c.getKeys = append(c.getKeys, sessionHash)
	if err := c.getErrors[sessionHash]; err != nil {
		return 0, err
	}
	return c.stubGatewayCache.GetSessionAccountID(ctx, groupID, sessionHash)
}

func TestGetStickySessionAccountID_FallbackToLegacyKey(t *testing.T) {
	beforeFallbackTotal, beforeFallbackHit, _ := openAIStickyCompatStats()

	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{
			"openai:legacy-hash": 42,
		},
	}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIWS: config.GatewayOpenAIWSConfig{
					SessionHashReadOldFallback: true,
				},
			},
		},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	accountID, err := svc.getStickySessionAccountID(ctx, nil, "new-hash")
	require.NoError(t, err)
	require.Equal(t, int64(42), accountID)

	afterFallbackTotal, afterFallbackHit, _ := openAIStickyCompatStats()
	require.Equal(t, beforeFallbackTotal+1, afterFallbackTotal)
	require.Equal(t, beforeFallbackHit+1, afterFallbackHit)
}

func TestGetStickySessionAccountID_PrimaryInfrastructureErrorSkipsLegacyFallback(t *testing.T) {
	primaryErr := errors.New("redis primary read failed")
	cache := &stickyCompatLookupCache{
		stubGatewayCache: &stubGatewayCache{sessionBindings: map[string]int64{
			"openai:legacy-hash": 42,
		}},
		getErrors: map[string]error{
			"openai:new-hash": primaryErr,
		},
	}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIWS: config.GatewayOpenAIWSConfig{
			SessionHashReadOldFallback: true,
		}}},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	accountID, err := svc.getStickySessionAccountID(ctx, nil, "new-hash")
	require.ErrorIs(t, err, primaryErr)
	require.Zero(t, accountID)
	require.Equal(t, []string{"openai:new-hash"}, cache.getKeys, "primary infrastructure errors must not be hidden by a legacy hit")
}

func TestGetStickySessionAccountID_PrimaryMissLegacyInfrastructureErrorPropagates(t *testing.T) {
	legacyErr := errors.New("redis legacy read failed")
	cache := &stickyCompatLookupCache{
		stubGatewayCache: &stubGatewayCache{sessionBindings: map[string]int64{}},
		getErrors: map[string]error{
			"openai:legacy-hash": legacyErr,
		},
	}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIWS: config.GatewayOpenAIWSConfig{
			SessionHashReadOldFallback: true,
		}}},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	accountID, err := svc.getStickySessionAccountID(ctx, nil, "new-hash")
	require.ErrorIs(t, err, legacyErr)
	require.Zero(t, accountID)
	require.Equal(t, []string{"openai:new-hash", "openai:legacy-hash"}, cache.getKeys)
}

func TestGetStickySessionAccountID_PrimaryAndLegacyMissRemainCacheMiss(t *testing.T) {
	cache := &stickyCompatLookupCache{
		stubGatewayCache: &stubGatewayCache{sessionBindings: map[string]int64{}},
		getErrors:        map[string]error{},
	}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{Gateway: config.GatewayConfig{OpenAIWS: config.GatewayOpenAIWSConfig{
			SessionHashReadOldFallback: true,
		}}},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	accountID, err := svc.getStickySessionAccountID(ctx, nil, "new-hash")
	require.ErrorIs(t, err, ErrGatewayCacheMiss)
	require.Zero(t, accountID)
}

func TestSetStickySessionAccountID_DualWriteOldEnabled(t *testing.T) {
	_, _, beforeDualWriteTotal := openAIStickyCompatStats()

	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIWS: config.GatewayOpenAIWSConfig{
					SessionHashDualWriteOld: true,
				},
			},
		},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	err := svc.setStickySessionAccountID(ctx, nil, "new-hash", 9, openAIStickySessionDefaultTTL)
	require.NoError(t, err)
	require.Equal(t, int64(9), cache.sessionBindings["openai:new-hash"])
	require.Equal(t, int64(9), cache.sessionBindings["openai:legacy-hash"])

	_, _, afterDualWriteTotal := openAIStickyCompatStats()
	require.Equal(t, beforeDualWriteTotal+1, afterDualWriteTotal)
}

func TestSetStickySessionAccountID_DualWriteOldDisabled(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	svc := &OpenAIGatewayService{
		cache: cache,
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				OpenAIWS: config.GatewayOpenAIWSConfig{
					SessionHashDualWriteOld: false,
				},
			},
		},
	}

	ctx := withOpenAILegacySessionHash(context.Background(), "legacy-hash")
	err := svc.setStickySessionAccountID(ctx, nil, "new-hash", 9, openAIStickySessionDefaultTTL)
	require.NoError(t, err)
	require.Equal(t, int64(9), cache.sessionBindings["openai:new-hash"])
	_, exists := cache.sessionBindings["openai:legacy-hash"]
	require.False(t, exists)
}

func TestSnapshotOpenAICompatibilityFallbackMetrics(t *testing.T) {
	before := SnapshotOpenAICompatibilityFallbackMetrics()

	ctx := context.WithValue(context.Background(), ctxkey.ThinkingEnabled, true)
	_, _ = ThinkingEnabledFromContext(ctx)

	after := SnapshotOpenAICompatibilityFallbackMetrics()
	require.GreaterOrEqual(t, after.MetadataLegacyFallbackTotal, before.MetadataLegacyFallbackTotal+1)
	require.GreaterOrEqual(t, after.MetadataLegacyFallbackThinkingEnabledTotal, before.MetadataLegacyFallbackThinkingEnabledTotal+1)
}
