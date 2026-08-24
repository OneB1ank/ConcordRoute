package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestGatewayCacheSessionLookupMapsOnlyRedisMiss(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	cache := NewGatewayCache(client)

	_, err := cache.GetSessionAccountID(context.Background(), 1, "missing")
	require.ErrorIs(t, err, service.ErrGatewayCacheMiss)
	_, err = cache.GetSessionOwnerGroupID(context.Background(), 2, service.SessionIsolationSourceOpenAI, "missing")
	require.ErrorIs(t, err, service.ErrGatewayCacheMiss)

	require.NoError(t, client.Close())
	_, err = cache.GetSessionAccountID(context.Background(), 1, "infra-error")
	require.Error(t, err)
	require.False(t, errors.Is(err, service.ErrGatewayCacheMiss), "infrastructure failure must not be reported as a cache miss")
}
