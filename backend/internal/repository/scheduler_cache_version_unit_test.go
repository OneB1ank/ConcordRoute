//go:build unit

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/stretchr/testify/require"
)

// 覆盖纳秒精度、时区、无版本写入、删除和旧编码失败，防止旁路重新引入回退。
func TestSchedulerAccountVersionBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"nanosecond", "timezone_equal", "unversioned", "delete", "invalid_old", "repair", "legacy_writer"} {
		t.Run(mode, func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			base := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
			fresh := service.Account{ID: 992001, UpdatedAt: base, Name: "new"}
			require.NoError(t, cache.SetAccount(ctx, &fresh))
			old := fresh
			old.Name = "old"
			old.UpdatedAt = base.Add(-time.Nanosecond)
			switch mode {
			case "timezone_equal":
				old = fresh
				old.UpdatedAt = base.In(time.FixedZone("test", 8*3600))
			case "unversioned":
				old.UpdatedAt = time.Time{}
			case "delete":
				require.NoError(t, cache.DeleteAccount(ctx, fresh.ID))
			case "invalid_old":
				bad := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				old.ExpiresAt = &bad
			case "repair":
				bad := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
				invalid := fresh
				invalid.ExpiresAt = &bad
				require.NoError(t, cache.SetAccount(ctx, &invalid))
				missing, err := cache.GetAccount(ctx, fresh.ID)
				require.NoError(t, err)
				require.Nil(t, missing)
				old = fresh
			case "legacy_writer":
				require.NoError(t, cache.rdb.Set(ctx, "sched:acc:992001", `{"ID":992001,"Name":"legacy"}`, 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, "sched:meta:992001", `{"ID":992001,"Name":"legacy"}`, 0).Err())
			}
			require.NoError(t, cache.SetAccount(ctx, &old))
			got, err := cache.GetAccount(ctx, fresh.ID)
			require.NoError(t, err)
			if mode == "delete" {
				require.Nil(t, got)
			} else {
				require.NotNil(t, got)
				require.Equal(t, fresh.Name, got.Name)
				require.True(t, got.UpdatedAt.Equal(fresh.UpdatedAt))
			}
		})
	}
}

// 真正并发调用缓存写入入口，最终版本与账号载体必须来自同一次最新更新。
func TestSchedulerAccountConcurrentVersionPublication(t *testing.T) {
	cache := newSchedulerCacheUnit(t)
	ctx := context.Background()
	const count = 24
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(version int) {
			defer wg.Done()
			account := service.Account{ID: 992002, Name: fmt.Sprint(version), UpdatedAt: time.Unix(1700000000, int64(version))}
			errs <- cache.SetAccount(ctx, &account)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := cache.GetAccount(ctx, 992002)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, fmt.Sprint(count-1), got.Name)
	rawMeta, err := cache.rdb.Get(ctx, schedulerAccountMetaKey("992002")).Result()
	require.NoError(t, err)
	meta, err := decodeCachedAccount(rawMeta)
	require.NoError(t, err)
	require.Equal(t, got.Name, meta.Name)
}

// 使用真实 Redis 编解码及 miniredis，固定读写交错，不依赖随机并发调度。
func TestSchedulerFullAccountWriteOrder(t *testing.T) {
	for _, order := range []string{"old_then_new_control", "new_then_old", "bucket_refresh_after_new", "fenced_rebuild_after_new"} {
		t.Run(order, func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			ctx := context.Background()
			old := service.Account{
				ID: 991901, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true,
				UpdatedAt: time.Now().Add(-time.Minute),
				Extra:     map[string]any{"codex_fingerprint_mode": "cockpit"},
			}
			fresh := old
			fresh.UpdatedAt = old.UpdatedAt.Add(time.Second)
			fresh.Extra = map[string]any{
				"codex_fingerprint_mode":                 "cockpit",
				service.CodexIdentityBindingsExtraKey:    map[string]any{"existing-session": map[string]any{"uuid": "01993000-0000-7000-8000-000000000995"}},
				service.CodexTurnLineageBindingsExtraKey: map[string]any{"existing-turn": map[string]any{"uuid": "01993000-0000-7000-8000-000000000996"}},
			}
			first, second := &old, &fresh
			if order == "new_then_old" {
				first, second = &fresh, &old
			}
			require.NoError(t, cache.SetAccount(ctx, first))
			require.NoError(t, cache.SetAccount(ctx, second))
			if order == "bucket_refresh_after_new" || order == "fenced_rebuild_after_new" {
				bucket := service.SchedulerBucket{GroupID: 998891, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
				token, err := cache.CaptureBucketWriteToken(ctx, bucket)
				require.NoError(t, err)
				if order == "bucket_refresh_after_new" {
					// 已在库中更新账号后，之前读取的桶账号列表才写入共享完整账号键。
					require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{old}))
				} else {
					// 复用 SetSnapshot 的内部阶段，固定退休发生在分配版本后、账号写入前。
					version, err := cache.allocateSnapshotVersion(ctx, bucket, token)
					require.NoError(t, err)
					require.NoError(t, cache.RetireBucket(ctx, bucket))
					_, err = cache.ReopenBucket(ctx, bucket)
					require.NoError(t, err)
					_, err = cache.writeSnapshotVersionAndReturnAccountIDs(ctx, bucket, version, []service.Account{old})
					require.NoError(t, err)
					require.ErrorIs(t, cache.activateSnapshotVersion(ctx, bucket, token, version), service.ErrSchedulerBucketWriteFenced)
					t.Log("bucket_activation_fenced=true")
				}
			}
			result, err := cache.GetAccount(ctx, old.ID)
			require.NoError(t, err)
			require.NotNil(t, result)
			_, identityKept := result.Extra[service.CodexIdentityBindingsExtraKey]
			_, lineageKept := result.Extra[service.CodexTurnLineageBindingsExtraKey]
			t.Logf("order=%s latest_version_kept=%v identity_store_kept=%v lineage_store_kept=%v", order, result.UpdatedAt.Equal(fresh.UpdatedAt), identityKept, lineageKept)
			require.True(t, result.UpdatedAt.Equal(fresh.UpdatedAt) && identityKept && lineageKept, "旧完整账号晚写不应覆盖新完整账号")
		})
	}
}
