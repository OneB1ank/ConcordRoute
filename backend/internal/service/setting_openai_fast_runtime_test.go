package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 只实现本轮使用的存储方法，意外进入其它 DB 路径会直接暴露。
type ttftFastPolicyRepo struct {
	SettingRepository
	mu      sync.Mutex
	value   string
	err     error
	delay   time.Duration
	reads   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (r *ttftFastPolicyRepo) GetValue(ctx context.Context, _ string) (string, error) {
	r.reads.Add(1)
	r.mu.Lock()
	value, err, delay := r.value, r.err, r.delay
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- struct{}{}
		select {
		case <-r.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return value, err
}

func (r *ttftFastPolicyRepo) Set(_ context.Context, _, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.value = value
	return nil
}

func BenchmarkOpenAIFastPolicyHotPath(b *testing.B) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`, delay: 2 * time.Millisecond}
	svc := &OpenAIGatewayService{settingService: &SettingService{settingRepo: repo}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.evaluateOpenAIFastPolicy(context.Background(), account, "test-model", "priority")
	repo.reads.Store(0)
	b.ResetTimer()
	for range b.N {
		svc.evaluateOpenAIFastPolicy(context.Background(), account, "test-model", "priority")
	}
	b.StopTimer()
	b.ReportMetric(float64(repo.reads.Load())/float64(b.N), "db_reads/op")
}

// 写入成功后下一请求立即采用新策略，禁止用性能优化交换策略一致性。
func TestOpenAIFastPolicyRuntimeUpdate(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`}
	setting := &SettingService{settingRepo: repo}
	svc := &OpenAIGatewayService{settingService: setting}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	action, _ := svc.evaluateOpenAIFastPolicy(context.Background(), account, "test-model", "priority")
	require.Equal(t, BetaPolicyActionPass, action)
	require.NoError(t, setting.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Scope: BetaPolicyScopeAll, Action: BetaPolicyActionBlock}},
	}))
	action, _ = svc.evaluateOpenAIFastPolicy(context.Background(), account, "test-model", "priority")
	require.Equal(t, BetaPolicyActionBlock, action)
}

func TestOpenAIFastPolicyRuntimeConcurrentColdReads(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`, delay: 20 * time.Millisecond}
	setting := &SettingService{settingRepo: repo}
	errs := make(chan error, 24)
	start := make(chan struct{})
	for range cap(errs) {
		go func() {
			<-start
			_, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
			errs <- err
		}()
	}
	close(start)
	for range cap(errs) {
		require.NoError(t, <-errs)
	}
	require.EqualValues(t, 1, repo.reads.Load())
}

func TestOpenAIFastPolicyRuntimeClonesAndExpires(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[{"service_tier":"priority","action":"block","scope":"all","user_ids":[42],"model_whitelist":["test-*"]}]}`}
	setting := &SettingService{settingRepo: repo}
	first, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	first.Rules[0].Action = "pass"
	first.Rules[0].UserIDs[0] = 99
	first.Rules[0].ModelWhitelist[0] = "changed"
	second, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Equal(t, "block", second.Rules[0].Action)
	require.EqualValues(t, 42, second.Rules[0].UserIDs[0])
	require.Equal(t, "test-*", second.Rules[0].ModelWhitelist[0])

	// 模拟其它节点直接更新 DB；管理读取直读，运行态在 TTL 后刷新。
	require.NoError(t, repo.Set(context.Background(), "", `{"rules":[]}`))
	admin, err := setting.GetOpenAIFastPolicySettings(context.Background())
	require.NoError(t, err)
	require.Empty(t, admin.Rules)
	setting.openAIFastPolicyRuntime.mu.Lock()
	setting.openAIFastPolicyRuntime.expiresAt = time.Now().Add(-time.Second)
	setting.openAIFastPolicyRuntime.mu.Unlock()
	expired, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Empty(t, expired.Rules)
	require.EqualValues(t, 3, repo.reads.Load())
}

func TestOpenAIFastPolicyRuntimeOldReadCannotOverwriteSave(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`, entered: make(chan struct{}, 2), release: make(chan struct{})}
	setting := &SettingService{settingRepo: repo}
	type outcome struct {
		settings *OpenAIFastPolicySettings
		err      error
	}
	result := make(chan outcome, 1)
	go func() {
		s, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
		result <- outcome{s, err}
	}()
	<-repo.entered
	require.NoError(t, setting.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Scope: BetaPolicyScopeAll, Action: BetaPolicyActionBlock}},
	}))
	close(repo.release)
	r := <-result
	require.NoError(t, r.err)
	require.Len(t, r.settings.Rules, 1)
	require.Equal(t, BetaPolicyActionBlock, r.settings.Rules[0].Action)
	latest, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Equal(t, BetaPolicyActionBlock, latest.Rules[0].Action)
	require.EqualValues(t, 1, repo.reads.Load(), "保存发布的新快照不需要再次回源")
}

func TestOpenAIFastPolicyRuntimeCancellationDoesNotPoisonSharedRead(t *testing.T) {
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`, entered: make(chan struct{}, 2), release: make(chan struct{})}
	setting := &SettingService{settingRepo: repo}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := setting.getOpenAIFastPolicySettingsCached(ctx)
		result <- err
	}()
	<-repo.entered
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("取消的调用者仍在等待数据库")
	}
	close(repo.release)
	_, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, repo.reads.Load())
}

func TestOpenAIFastPolicyRuntimeErrorsAndInstanceIsolation(t *testing.T) {
	dbErr := errors.New("synthetic DB error")
	repo := &ttftFastPolicyRepo{value: `{"rules":[]}`, err: dbErr}
	setting := &SettingService{settingRepo: repo}
	_, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.ErrorIs(t, err, dbErr)
	repo.mu.Lock()
	repo.err = nil
	repo.mu.Unlock()
	got, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Empty(t, got.Rules)
	require.EqualValues(t, 2, repo.reads.Load(), "数据库错误不能把默认放行策略永久缓存")

	other := &SettingService{settingRepo: &ttftFastPolicyRepo{value: `{"rules":[{"action":"block"}]}`}}
	independent, err := other.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Len(t, independent.Rules, 1)

	repo.mu.Lock()
	repo.err = dbErr
	repo.mu.Unlock()
	require.ErrorIs(t, setting.SetOpenAIFastPolicySettings(context.Background(), &OpenAIFastPolicySettings{
		Rules: []OpenAIFastPolicyRule{{ServiceTier: OpenAIFastTierPriority, Scope: BetaPolicyScopeAll, Action: BetaPolicyActionBlock}},
	}), dbErr)
	current, err := setting.getOpenAIFastPolicySettingsCached(context.Background())
	require.NoError(t, err)
	require.Empty(t, current.Rules, "失败的保存不应发布到运行态缓存")
}
