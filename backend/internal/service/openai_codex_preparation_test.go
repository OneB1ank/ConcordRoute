package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 经真实转发入口覆盖普通、透传、旧压缩和 Messages，取消必须在持锁请求释放前生效。
func TestCodexPreparationHTTPPathsCancelBeforeGeneration(t *testing.T) {
	for i, route := range []string{"responses", "raw", "compact", "raw_compact", "messages"} {
		t.Run(route, func(t *testing.T) {
			account := newTestOAuthAccount(9971000+int64(i), map[string]any{
				codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": strings.HasPrefix(route, "raw"),
			})
			repo := &codexBindingsReaderProbe{account: account}
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
			body := []byte(`{"model":"gpt-6-astra","instructions":"synthetic test","input":[],"stream":true,"client_metadata":{"session_id":"session_test","thread_id":"thread_test"}}`)
			path := "/v1/responses"
			if strings.Contains(route, "compact") {
				path += "/compact"
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
			lock := codexIdentityBindingLock(account.ID)
			lock.Lock()
			done := make(chan error, 1)
			go func() {
				if route == "messages" {
					_, _, err := svc.prepareMessagesCodexFingerprint(ctx, c, account, body, "")
					done <- err
				} else {
					_, err := svc.Forward(ctx, c, account, body)
					done <- err
				}
			}()
			select {
			case err := <-done:
				lock.Unlock()
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(250 * time.Millisecond):
				lock.Unlock()
				<-done
				t.Fatal("身份生成等待没有响应请求截止时间")
			}
			require.Zero(t, repo.reads.Load())
			require.Zero(t, repo.writes.Load())
			require.Empty(t, upstream.lastBody)
			require.False(t, account.codexIdentityLockHeld)
		})
	}
}

// 回调内部发生取消时不得继续落库；失败后原账号没有残留锁标记，可正常重试。
func TestCodexPreparationCancellationAfterResolve(t *testing.T) {
	account := newTestOAuthAccount(9971010, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	repo := &codexBindingsReaderProbe{account: account}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := prepareCodexFingerprint(ctx, repo, account, func(local *Account) *codexFingerprintIDs {
		require.True(t, local.codexIdentityLockHeld)
		require.NotSame(t, account, local)
		cancel()
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, repo.reads.Load())
	require.False(t, account.codexIdentityLockHeld)
	_, err = prepareCodexFingerprint(context.Background(), repo, account, func(*Account) *codexFingerprintIDs { return nil })
	require.NoError(t, err)
}

// 关闭身份映射且没有仓储工作时，准备层不改变流式断连后的读取用量契约。
func TestCodexPreparationDisabledPreservesNoWorkLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, account := range []*Account{nil, newTestOAuthAccount(9971013, nil)} {
		called := false
		ids, err := prepareCodexFingerprint(ctx, nil, account, func(local *Account) *codexFingerprintIDs {
			called = true
			require.Equal(t, account, local)
			return nil
		})
		require.NoError(t, err)
		require.Nil(t, ids)
		require.True(t, called)
	}
}

// 仅验证既有探测的本地准备阶段，既不访问上游，也不调度额度查询。
func TestCodexPreparationProbeCancellation(t *testing.T) {
	for i, purpose := range []codexProbePurpose{codexProbePurposeAccountTest, codexProbePurposeNativeCompactionV2, codexProbePurposeImageAccountTest, codexProbePurposeQuotaOverdraft} {
		t.Run(string(purpose), func(t *testing.T) {
			account := newTestOAuthAccount(9971020+int64(i), map[string]any{codexFingerprintModeExtraKey: "cockpit"})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			lock := codexIdentityBindingLock(account.ID)
			lock.Lock()
			done := make(chan error, 1)
			go func() {
				_, err := prepareCodexProbeFingerprint(ctx, account, purpose, "synthetic-model")
				done <- err
			}()
			select {
			case err := <-done:
				lock.Unlock()
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(250 * time.Millisecond):
				lock.Unlock()
				<-done
				t.Fatal("探测身份准备没有响应取消")
			}
			require.False(t, account.codexIdentityLockHeld)
		})
	}
}

// WS 取消和客户端切换分别保持服务错误与策略错误，不提交失败帧的连接状态。
func TestCodexPreparationWebSocketCancellationAndState(t *testing.T) {
	account := newTestOAuthAccount(9971011, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	body := []byte(`{"client_metadata":{"session_id":"session","thread_id":"thread","turn_id":"turn","window_number":0}}`)
	headers := http.Header{"Version": []string{"0.153.3"}}
	ids := resolveCodexFingerprintIDsFromRawRequest(account, headers, body)
	state := newCodexWebSocketFingerprintState(account, ids, headers, body)
	before := *state
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lock := codexIdentityBindingLock(account.ID)
	lock.Lock()
	done := make(chan error, 1)
	go func() { _, err := state.prepare(ctx, nil, body); done <- err }()
	var err error
	select {
	case err = <-done:
		lock.Unlock()
	case <-time.After(250 * time.Millisecond):
		lock.Unlock()
		<-done
		t.Fatal("WS 帧准备等待没有响应取消")
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.statusCode)
	require.Equal(t, before, *state)
	_, err = state.prepare(context.Background(), nil, []byte(`{"client_metadata":{"session_id":"different-session"}}`))
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.statusCode)
	require.Equal(t, before, *state)
}

// JSON 读取后的规范字符串无需重复分配；历史大小写、紧凑和带空白形式仍正常规范化。
func TestCodexBindingUUIDNormalizationAndHotAllocation(t *testing.T) {
	const canonical = "01993000-0000-7000-8000-00000000aabb"
	for _, value := range []string{canonical, strings.ToUpper(canonical), "{" + canonical + "}", "  " + strings.ReplaceAll(canonical, "-", "") + "  "} {
		raw := map[string]any{"uuid": value, "created_at_ms": float64(10), "last_used_at_ms": float64(20)}
		binding, ok := parseCodexIdentityBinding(raw)
		require.True(t, ok)
		require.Equal(t, canonical, binding.UUID)
	}
	raw := map[string]any{"uuid": canonical, "created_at_ms": float64(10), "last_used_at_ms": float64(20)}
	allocs := testing.AllocsPerRun(1000, func() {
		_, _ = parseCodexIdentityBinding(raw)
	})
	require.Zero(t, allocs, "规范 JSON 绑定热解析不应重新格式化 UUID")
}

// 即使命中旧成功哈希，也必须依据最新数据库值判断是否仍需写入。
func TestCodexPersistenceFastPathUsesLatestDurableState(t *testing.T) {
	account := newTestOAuthAccount(9971012, nil)
	now := time.Now().UnixMilli()
	binding := codexIdentityBinding{UUID: newCodexUUIDv7().String(), CreatedAtMS: now - 1000, LastUsedAtMS: now}
	account.Extra[CodexIdentityBindingsExtraKey] = map[string]any{"active": binding}
	encoded, err := json.Marshal(account.Extra)
	require.NoError(t, err)
	var extra map[string]any
	require.NoError(t, json.Unmarshal(encoded, &extra))
	repo := &codexBindingsReaderProbe{account: &Account{ID: account.ID, Extra: extra}}
	t.Cleanup(func() { codexIdentityPersistedHashes.Delete(fmt.Sprint(account.ID)) })
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 1, repo.writes.Load())
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 1, repo.writes.Load())

	// 用独立映射模拟另一实例恢复旧值，避免共享测试 map 掩盖数据差异。
	old := binding
	old.UUID = newCodexUUIDv7().String()
	old.LastUsedAtMS--
	repo.account.Extra[CodexIdentityBindingsExtraKey] = map[string]any{"active": old}
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 2, repo.writes.Load())
	stored, _ := parseCodexIdentityBinding(readCodexIdentityBindings(repo.account)["active"])
	require.Equal(t, binding, stored)

	// 清理过期持久化项不能被“裁剪后恰好相等”误判为零写入。
	expired := binding
	expired.LastUsedAtMS = now - codexIdentityBindingIdleTTL.Milliseconds() - 1
	repo.account.Extra[CodexIdentityBindingsExtraKey] = map[string]any{"active": binding, "expired": expired}
	require.NoError(t, persistCodexIdentityBindings(context.Background(), repo, account))
	require.EqualValues(t, 3, repo.writes.Load())
	require.NotContains(t, readCodexIdentityBindings(repo.account), "expired")
}
