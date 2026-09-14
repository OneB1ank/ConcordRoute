package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 窄读取仓储刻意拒绝完整账号读取，防止优化后又加载代理、分组等关联。
type codexBindingsReaderProbe struct {
	AccountRepository
	account  *Account
	reads    atomic.Int32
	writes   atomic.Int32
	read     func(context.Context) error
	writeErr error
}

func (r *codexBindingsReaderProbe) GetByID(context.Context, int64) (*Account, error) {
	return nil, errors.New("unexpected full account read")
}

func (r *codexBindingsReaderProbe) GetCodexIdentityBindings(ctx context.Context, _ int64) (*Account, error) {
	r.reads.Add(1)
	if r.read != nil {
		if err := r.read(ctx); err != nil {
			return nil, err
		}
	}
	return r.account, nil
}

func (r *codexBindingsReaderProbe) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.writes.Add(1)
	if r.writeErr != nil {
		return r.writeErr
	}
	for key, value := range updates {
		r.account.Extra[key] = value
	}
	return nil
}

func persistenceLatencyAccount(id int64) *Account {
	// 空集合也需要持久化，用于清除已过期的旧绑定。
	return &Account{ID: id, Extra: map[string]any{CodexIdentityBindingsExtraKey: map[string]any{}}}
}

func TestPersistCodexIdentityBindings_CanceledLockWaitReturnsBeforeUnlock(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "canceled", true: "deadline"}[deadline], func(t *testing.T) {
			account := persistenceLatencyAccount(982001)
			lock := codexIdentityBindingLock(account.ID)
			lock.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			} else {
				cancel()
			}
			defer cancel()
			done := make(chan error, 1)
			repo := &codexBindingsReaderProbe{account: account}
			go func() { done <- persistCodexIdentityBindings(ctx, repo, account) }()
			select {
			case err := <-done:
				lock.Unlock()
				require.ErrorIs(t, err, ctx.Err())
			case <-time.After(200 * time.Millisecond):
				// 即使旧实现失败也先释放锁、回收协程，避免污染后续测试。
				lock.Unlock()
				<-done
				t.Fatal("已取消的持久化请求仍在等待账号锁")
			}
			require.Zero(t, repo.reads.Load())
			require.Zero(t, repo.writes.Load())
		})
	}
}

func TestPersistCodexIdentityBindings_NarrowReadKeepsDurabilityAndBudget(t *testing.T) {
	account := persistenceLatencyAccount(982002)
	codexIdentityPersistedHashes.Delete("982002")
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete("982002") })
	repo := &codexBindingsReaderProbe{account: account}
	repo.read = func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			return errors.New("missing shared persistence deadline")
		}
		return ctx.Err()
	}
	for range 3 {
		require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	}
	require.EqualValues(t, 3, repo.reads.Load(), "仍读取最新持久化状态，不以进程缓存代替数据库")
	require.EqualValues(t, 1, repo.writes.Load(), "相同集合继续减少重复写入")

	// 模拟重启丢失进程哈希后，仍可读取并保存相同状态。
	codexIdentityPersistedHashes.Delete("982002")
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 2, repo.writes.Load())
}

func TestPersistCodexIdentityBindings_NarrowReadFailureAndWriteRetry(t *testing.T) {
	account := persistenceLatencyAccount(982003)
	codexIdentityPersistedHashes.Delete("982003")
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete("982003") })
	dbErr := errors.New("storage unavailable")
	repo := &codexBindingsReaderProbe{account: account, read: func(context.Context) error { return dbErr }}
	require.ErrorIs(t, persistCodexIdentityBindings(context.Background(), repo, account), dbErr)
	require.Zero(t, repo.writes.Load())
	repo.read = nil
	repo.writeErr = dbErr
	require.ErrorIs(t, persistCodexIdentityBindings(context.Background(), repo, account), dbErr)
	_, cached := codexIdentityPersistedHashes.Load("982003")
	require.False(t, cached, "落库失败不能登记成功哈希")
	repo.writeErr = nil
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 2, repo.writes.Load(), "失败后必须重新写入")
}

func TestPersistCodexIdentityBindings_StorageCancellationAndTrace(t *testing.T) {
	account := persistenceLatencyAccount(982004)
	codexIdentityPersistedHashes.Delete("982004")
	rec := latencytrace.New(time.Now())
	ctx, cancel := context.WithTimeout(latencytrace.WithRecorder(context.Background(), rec), 20*time.Millisecond)
	defer cancel()
	repo := &codexBindingsReaderProbe{account: account, read: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	require.ErrorIs(t, persistCodexIdentityBindings(ctx, repo, account), context.DeadlineExceeded)
	require.Zero(t, repo.writes.Load())
	events := rec.Snapshot().Events
	require.Len(t, events, 4)
	require.Equal(t, "identity_lock_wait_started", events[0].Phase)
	require.Equal(t, "identity_lock_wait_done", events[1].Phase)
	require.Equal(t, "identity_binding_read_started", events[2].Phase)
	require.Equal(t, "identity_binding_read_done", events[3].Phase)
	require.True(t, events[3].Failed)
	// 存储取消后账号锁必须释放，后续正常请求仍会完成落库。
	repo.read = nil
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 1, repo.writes.Load())
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete("982004") })
}

// 在真实 Forward 入口验证：落库读/写失败时，普通及透传路径均不会发出模型请求。
func TestCodexPersistenceFailureStopsHTTPForward(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, writeFailure := range []bool{false, true} {
			account := newTestOAuthAccount(982005, map[string]any{
				codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
				CodexIdentityBindingsExtraKey: map[string]any{},
			})
			codexIdentityPersistedHashes.Delete("982005")
			upstream := &httpUpstreamRecorder{}
			dbErr := errors.New("persistence unavailable")
			repo := &codexBindingsReaderProbe{account: account}
			if writeFailure {
				repo.writeErr = dbErr
			} else {
				repo.read = func(context.Context) error { return dbErr }
			}
			svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
			body := []byte(`{"model":"gpt-5.4","instructions":"test","input":[],"stream":true}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			result, err := svc.Forward(context.Background(), c, account, body)
			require.ErrorIs(t, err, dbErr)
			require.Nil(t, result)
			require.Empty(t, upstream.lastBody, "存储失败应在 HTTP Do 之前返回")
		}
	}
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete("982005") })
}
