package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSStateStore_BindGetDeleteResponseAccount(t *testing.T) {
	cache := &stubGatewayCache{}
	store := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	groupID := int64(7)

	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_abc", 101, time.Minute))

	accountID, err := store.GetResponseAccount(ctx, groupID, "resp_abc")
	require.NoError(t, err)
	require.Equal(t, int64(101), accountID)

	require.NoError(t, store.DeleteResponseAccount(ctx, groupID, "resp_abc"))
	accountID, err = store.GetResponseAccount(ctx, groupID, "resp_abc")
	require.NoError(t, err)
	require.Zero(t, accountID)
}

func TestOpenAIWSStateStore_ResponseAccountLocalCacheIsGroupScoped(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	ctx := context.Background()

	require.NoError(t, store.BindResponseAccount(ctx, 7, "resp_shared", 101, time.Minute))

	accountID, err := store.GetResponseAccount(ctx, 7, "resp_shared")
	require.NoError(t, err)
	require.Equal(t, int64(101), accountID)

	accountID, err = store.GetResponseAccount(ctx, 8, "resp_shared")
	require.NoError(t, err)
	require.Zero(t, accountID, "本地 response 绑定必须按 group 隔离，避免跨组命中")

	require.NoError(t, store.BindResponseAccount(ctx, 8, "resp_shared", 202, time.Minute))
	accountID, err = store.GetResponseAccount(ctx, 8, "resp_shared")
	require.NoError(t, err)
	require.Equal(t, int64(202), accountID)

	require.NoError(t, store.DeleteResponseAccount(ctx, 7, "resp_shared"))
	accountID, err = store.GetResponseAccount(ctx, 8, "resp_shared")
	require.NoError(t, err)
	require.Equal(t, int64(202), accountID, "删除某个 group 的绑定不应影响其它 group")
}

func TestOpenAIWSStateStore_ResponseConnTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindResponseConn("resp_conn", "conn_1", 30*time.Millisecond)

	connID, ok := store.GetResponseConn("resp_conn")
	require.True(t, ok)
	require.Equal(t, "conn_1", connID)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetResponseConn("resp_conn")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_SessionTurnStateTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindSessionTurnState(9, "session_hash_1", "turn_state_1", 30*time.Millisecond)

	state, ok := store.GetSessionTurnState(9, "session_hash_1")
	require.True(t, ok)
	require.Equal(t, "turn_state_1", state)

	// group 隔离
	_, ok = store.GetSessionTurnState(10, "session_hash_1")
	require.False(t, ok)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetSessionTurnState(9, "session_hash_1")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_SessionConnTTL(t *testing.T) {
	store := NewOpenAIWSStateStore(nil)
	store.BindSessionConn(9, "session_hash_conn_1", "conn_1", 30*time.Millisecond)

	connID, ok := store.GetSessionConn(9, "session_hash_conn_1")
	require.True(t, ok)
	require.Equal(t, "conn_1", connID)

	// group 隔离
	_, ok = store.GetSessionConn(10, "session_hash_conn_1")
	require.False(t, ok)

	time.Sleep(60 * time.Millisecond)
	_, ok = store.GetSessionConn(9, "session_hash_conn_1")
	require.False(t, ok)
}

func TestOpenAIWSStateStore_GetResponseAccount_NoStaleAfterCacheMiss(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	store := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	groupID := int64(17)
	responseID := "resp_cache_stale"
	cacheKey := openAIWSResponseAccountCacheKey(responseID)

	cache.sessionBindings[cacheKey] = 501
	accountID, err := store.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Equal(t, int64(501), accountID)

	delete(cache.sessionBindings, cacheKey)
	accountID, err = store.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Zero(t, accountID, "上游缓存失效后不应继续命中本地陈旧映射")
}

func TestOpenAIWSStateStore_SharedCacheDeleteInvalidatesOtherStoreLocalBinding(t *testing.T) {
	cache := &stubGatewayCache{sessionBindings: map[string]int64{}}
	writerStore := NewOpenAIWSStateStore(cache)
	deleterStore := NewOpenAIWSStateStore(cache)
	ctx := context.Background()
	groupID := int64(18)
	responseID := "resp_cross_instance_delete"

	require.NoError(t, writerStore.BindResponseAccount(ctx, groupID, responseID, 601, time.Minute))
	accountID, err := writerStore.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Equal(t, int64(601), accountID)

	require.NoError(t, deleterStore.DeleteResponseAccount(ctx, groupID, responseID))
	accountID, err = writerStore.GetResponseAccount(ctx, groupID, responseID)
	require.NoError(t, err)
	require.Zero(t, accountID, "共享缓存删除后其它实例不得继续返回本地陈旧绑定")

	rawWriterStore, ok := writerStore.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	rawWriterStore.responseToAccountMu.RLock()
	_, localExists := rawWriterStore.responseToAccount[openAIWSResponseAccountMapKey(groupID, responseID)]
	rawWriterStore.responseToAccountMu.RUnlock()
	require.False(t, localExists, "共享缓存 miss 后应清理本地陈旧镜像")
}

func TestOpenAIWSStateStore_MaybeCleanupRemovesExpiredIncrementally(t *testing.T) {
	raw := NewOpenAIWSStateStore(nil)
	store, ok := raw.(*defaultOpenAIWSStateStore)
	require.True(t, ok)

	expiredAt := time.Now().Add(-time.Minute)
	total := 2048
	store.responseToConnMu.Lock()
	for i := 0; i < total; i++ {
		store.responseToConn[fmt.Sprintf("resp_%d", i)] = openAIWSConnBinding{
			connID:    "conn_incremental",
			expiresAt: expiredAt,
		}
	}
	store.responseToConnMu.Unlock()

	store.lastCleanupUnixNano.Store(time.Now().Add(-2 * openAIWSStateStoreCleanupInterval).UnixNano())
	store.maybeCleanup()

	store.responseToConnMu.RLock()
	remainingAfterFirst := len(store.responseToConn)
	store.responseToConnMu.RUnlock()
	require.Less(t, remainingAfterFirst, total, "单轮 cleanup 应至少有进展")
	require.Greater(t, remainingAfterFirst, 0, "增量清理不要求单轮清空全部键")

	for i := 0; i < 8; i++ {
		store.lastCleanupUnixNano.Store(time.Now().Add(-2 * openAIWSStateStoreCleanupInterval).UnixNano())
		store.maybeCleanup()
	}

	store.responseToConnMu.RLock()
	remaining := len(store.responseToConn)
	store.responseToConnMu.RUnlock()
	require.Zero(t, remaining, "多轮 cleanup 后应逐步清空全部过期键")
}

func TestEnsureBindingCapacity_EvictsOneWhenMapIsFull(t *testing.T) {
	bindings := map[string]int{
		"a": 1,
		"b": 2,
	}

	ensureBindingCapacity(bindings, "c", 2)
	bindings["c"] = 3

	require.Len(t, bindings, 2)
	require.Equal(t, 3, bindings["c"])
}

func TestEnsureBindingCapacity_DoesNotEvictWhenUpdatingExistingKey(t *testing.T) {
	bindings := map[string]int{
		"a": 1,
		"b": 2,
	}

	ensureBindingCapacity(bindings, "a", 2)
	bindings["a"] = 9

	require.Len(t, bindings, 2)
	require.Equal(t, 9, bindings["a"])
}

type openAIWSStateStoreTimeoutProbeCache struct {
	setHasDeadline    bool
	getHasDeadline    bool
	deleteHasDeadline bool
	setDeadlineDelta  time.Duration
	getDeadlineDelta  time.Duration
	delDeadlineDelta  time.Duration
	getAccountID      int64
	getErr            error
}

func (c *openAIWSStateStoreTimeoutProbeCache) GetSessionAccountID(ctx context.Context, _ int64, _ string) (int64, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.getHasDeadline = true
		c.getDeadlineDelta = time.Until(deadline)
	}
	if c.getErr != nil {
		return 0, c.getErr
	}
	if c.getAccountID > 0 {
		return c.getAccountID, nil
	}
	return 123, nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) SetSessionAccountID(ctx context.Context, _ int64, _ string, _ int64, _ time.Duration) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.setHasDeadline = true
		c.setDeadlineDelta = time.Until(deadline)
	}
	return errors.New("set failed")
}

func (c *openAIWSStateStoreTimeoutProbeCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) DeleteSessionAccountID(ctx context.Context, _ int64, _ string) error {
	if deadline, ok := ctx.Deadline(); ok {
		c.deleteHasDeadline = true
		c.delDeadlineDelta = time.Until(deadline)
	}
	return nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) SetSessionOwnerGroupID(context.Context, int64, string, string, int64, time.Duration) (bool, error) {
	return true, nil
}

func (c *openAIWSStateStoreTimeoutProbeCache) GetSessionOwnerGroupID(context.Context, int64, string, string) (int64, error) {
	return 0, errors.New("not found")
}

func (c *openAIWSStateStoreTimeoutProbeCache) RefreshSessionOwnerTTL(context.Context, int64, string, string, time.Duration) error {
	return nil
}

func TestOpenAIWSStateStore_RedisOpsUseShortTimeout(t *testing.T) {
	probe := &openAIWSStateStoreTimeoutProbeCache{}
	store := NewOpenAIWSStateStore(probe)
	ctx := context.Background()
	groupID := int64(5)

	err := store.BindResponseAccount(ctx, groupID, "resp_timeout_probe", 11, time.Minute)
	require.Error(t, err)

	accountID, getErr := store.GetResponseAccount(ctx, groupID, "resp_timeout_probe")
	require.NoError(t, getErr)
	require.Equal(t, int64(123), accountID, "配置共享缓存后应以缓存读取结果为准")

	require.NoError(t, store.DeleteResponseAccount(ctx, groupID, "resp_timeout_probe"))

	require.True(t, probe.setHasDeadline, "SetSessionAccountID 应携带独立超时上下文")
	require.True(t, probe.getHasDeadline, "GetSessionAccountID 应携带独立超时上下文")
	require.True(t, probe.deleteHasDeadline, "DeleteSessionAccountID 应携带独立超时上下文")
	require.Greater(t, probe.setDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.setDeadlineDelta, 3*time.Second)
	require.Greater(t, probe.getDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.getDeadlineDelta, 3*time.Second)
	require.Greater(t, probe.delDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe.delDeadlineDelta, 3*time.Second)

	probe2 := &openAIWSStateStoreTimeoutProbeCache{}
	store2 := NewOpenAIWSStateStore(probe2)
	accountID2, err2 := store2.GetResponseAccount(ctx, groupID, "resp_cache_only")
	require.NoError(t, err2)
	require.Equal(t, int64(123), accountID2)
	require.True(t, probe2.getHasDeadline, "GetSessionAccountID 在缓存未命中时应携带独立超时上下文")
	require.Greater(t, probe2.getDeadlineDelta, 2*time.Second)
	require.LessOrEqual(t, probe2.getDeadlineDelta, 3*time.Second)
}

func TestWithOpenAIWSStateStoreRedisTimeout_WithParentContext(t *testing.T) {
	ctx, cancel := withOpenAIWSStateStoreRedisTimeout(context.Background())
	defer cancel()
	require.NotNil(t, ctx)
	_, ok := ctx.Deadline()
	require.True(t, ok, "应附加短超时")
}

func TestOpenAIWSStateStore_GetResponseAccountDistinguishesMissFromFailure(t *testing.T) {
	t.Run("cache miss", func(t *testing.T) {
		store := NewOpenAIWSStateStore(&openAIWSStateStoreTimeoutProbeCache{getErr: ErrGatewayCacheMiss})
		accountID, err := store.GetResponseAccount(context.Background(), 7, "resp_missing")
		require.NoError(t, err)
		require.Zero(t, accountID)
	})

	t.Run("infrastructure failure", func(t *testing.T) {
		cacheErr := errors.New("redis transport failed")
		store := NewOpenAIWSStateStore(&openAIWSStateStoreTimeoutProbeCache{getErr: cacheErr})
		accountID, err := store.GetResponseAccount(context.Background(), 7, "resp_unknown")
		require.Zero(t, accountID)
		require.ErrorIs(t, err, cacheErr)
	})
}

func TestOpenAIWSStateStore_LocalResponseBindingDoesNotMaskCacheFailure(t *testing.T) {
	cacheErr := errors.New("redis unavailable")
	cache := &openAIWSStateStoreTimeoutProbeCache{getErr: cacheErr}
	store := NewOpenAIWSStateStore(cache)
	rawStore, ok := store.(*defaultOpenAIWSStateStore)
	require.True(t, ok)
	rawStore.responseToAccount[openAIWSResponseAccountMapKey(8, "resp_hot")] = openAIWSAccountBinding{
		accountID: 88,
		expiresAt: time.Now().Add(time.Minute),
	}

	accountID, err := store.GetResponseAccount(context.Background(), 8, "resp_hot")
	require.Zero(t, accountID)
	require.ErrorIs(t, err, cacheErr)
	require.True(t, cache.getHasDeadline, "共享缓存基础设施错误不得由潜在陈旧的本地绑定掩盖")
}

func TestClearOpenAIPreviousResponseBindingsUsesFinalResolvedGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalGroupID := int64(71)
	fallbackGroupID := int64(72)
	store := NewOpenAIWSStateStore(nil)
	require.NoError(t, store.BindResponseAccount(context.Background(), originalGroupID, "resp_final_group", 701, time.Hour))
	require.NoError(t, store.BindResponseAccount(context.Background(), fallbackGroupID, "resp_final_group", 702, time.Hour))
	store.BindResponseConn("resp_final_group", "conn_final_group", time.Hour)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	request = request.WithContext(context.WithValue(request.Context(), ctxkey.Group, &Group{
		ID:              originalGroupID,
		Platform:        PlatformOpenAI,
		Status:          StatusActive,
		Hydrated:        true,
		ClaudeCodeOnly:  true,
		FallbackGroupID: &fallbackGroupID,
	}))
	c.Request = request
	c.Set("api_key", &APIKey{GroupID: &originalGroupID})

	svc := &OpenAIGatewayService{openaiWSStateStore: store}
	require.NoError(t, svc.clearOpenAIPreviousResponseBindings(context.Background(), c, "resp_final_group"))

	fallbackBinding, err := store.GetResponseAccount(context.Background(), fallbackGroupID, "resp_final_group")
	require.NoError(t, err)
	require.Zero(t, fallbackBinding)
	originalBinding, err := store.GetResponseAccount(context.Background(), originalGroupID, "resp_final_group")
	require.NoError(t, err)
	require.Equal(t, int64(701), originalBinding, "cleanup must not delete the original pre-fallback group binding")
	_, connBound := store.GetResponseConn("resp_final_group")
	require.False(t, connBound)
}
