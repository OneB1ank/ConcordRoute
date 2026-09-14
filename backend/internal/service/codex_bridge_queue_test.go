package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 构造活动连接的绑定状态，不生成或伪造任何设备证明。
func bridgeQueueFixture(t *testing.T) (*OpenAIGatewayService, *codexAppServerBridge) {
	t.Helper()
	svc := &OpenAIGatewayService{codexAppServerBridges: newCodexAppServerBridgeRegistry()}
	b := &codexAppServerBridge{apiKeyID: 42, connectionID: "queue-test", sessionID: "session", closed: make(chan struct{})}
	b.negotiated.Store(true)
	b.initialized.Store(true)
	b.boundAccountID.Store(99)
	b.initLocks()
	require.NoError(t, svc.codexAppServerBridges.add(b))
	t.Cleanup(func() { svc.codexAppServerBridges.remove(b) })
	return svc, b
}

// 不匹配的账号应在等待互斥之前退出，即使当前连接正忙。
func TestCodexBridgeQueueDifferentAccountSkipsBusyLock(t *testing.T) {
	svc, b := bridgeQueueFixture(t)
	require.NoError(t, b.roundMu.Lock(context.Background()))
	defer b.roundMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	result, err := svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42,
		&Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "session", "")
	require.NoError(t, err)
	require.Same(t, ctx, result)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}

// 即便业务请求没有 deadline，证明排队也受到独立的两秒总预算约束。
func TestCodexBridgeQueueBoundedWithoutCallerDeadline(t *testing.T) {
	svc, b := bridgeQueueFixture(t)
	require.NoError(t, b.roundMu.Lock(context.Background()))
	defer b.roundMu.Unlock()
	start := time.Now()
	result, err := svc.bindCodexAppServerAttestationContextForAPIKey(context.Background(), 42,
		&Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "session", "")
	require.NoError(t, err)
	require.NoError(t, result.Err())
	require.GreaterOrEqual(t, time.Since(start), codexAppServerBridgeAttestationTimeout)
	require.Less(t, time.Since(start), codexAppServerBridgeAttestationTimeout+time.Second)
}

// 已取消的请求不应趁锁空闲时抢占绑定或发起 RPC。
func TestCodexBridgeQueueAlreadyCanceledDoesNotBind(t *testing.T) {
	svc, b := bridgeQueueFixture(t)
	b.boundAccountID.Store(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.bindCodexAppServerAttestationContextForAPIKey(ctx, 42,
		&Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "session", "")
	require.NoError(t, err)
	require.Zero(t, b.boundAccountID.Load())
}

// 本地 WebSocket 只接收 generate、不返回证明，用来验证真实写锁与 RPC 超时链路。
func bridgeQueueSocket(t *testing.T, b *codexAppServerBridge) *coderws.Conn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan *coderws.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
		<-ctx.Done()
	}))
	client, _, err := coderws.Dial(ctx, "ws"+server.URL[len("http"):], nil)
	if err != nil {
		cancel()
		server.Close()
		t.Fatal(err)
	}
	b.conn = <-accepted
	b.pending = make(map[string]chan []byte)
	t.Cleanup(func() {
		cancel()
		_ = client.CloseNow()
		_ = b.conn.CloseNow()
		server.Close()
	})
	return client
}

func TestCodexBridgeQueueAndRPCShareTotalBudget(t *testing.T) {
	svc, b := bridgeQueueFixture(t)
	svc.codexAttestationStore = liveattestation.NewAppServerAttestationStore(time.Minute)
	b.initializeRaw = []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`)
	client := bridgeQueueSocket(t, b)
	require.NoError(t, b.roundMu.Lock(context.Background()))
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(700 * time.Millisecond)
		b.roundMu.Unlock()
	}()
	received := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_, _, err := client.Read(ctx)
		received <- err
	}()
	start := time.Now()
	result, err := svc.bindCodexAppServerAttestationContextForAPIKey(context.Background(), 42,
		&Account{ID: 99, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, "session", "")
	elapsed := time.Since(start)
	<-released
	require.NoError(t, <-received, "必须实际发出 RPC，而不是在适配器初始化前退出")
	require.NoError(t, err)
	require.NoError(t, result.Err(), "业务上下文不能继承已结束的证明子上下文")
	require.GreaterOrEqual(t, elapsed, codexAppServerBridgeAttestationTimeout)
	require.Less(t, elapsed, codexAppServerBridgeAttestationTimeout+500*time.Millisecond,
		"排队 700ms 后，RPC 不应重新获得完整 2 秒预算")
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	require.Empty(t, b.pending, "超时后应清理等待中的请求")
}

func TestCodexBridgeWriteQueueHonorsCancellation(t *testing.T) {
	_, b := bridgeQueueFixture(t)
	_ = bridgeQueueSocket(t, b)
	require.NoError(t, b.writeMu.Lock(context.Background()))
	defer b.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.write(ctx, []byte(`{"id":1,"method":"test"}`))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 500*time.Millisecond)
}
