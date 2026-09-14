//go:build unit

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 复制完整账号，模拟仓储和 Redis 的反序列化边界。
func schedulerContinuityClone(t *testing.T, account *Account) *Account {
	t.Helper()
	data, err := json.Marshal(account)
	require.NoError(t, err)
	var cloned Account
	require.NoError(t, json.Unmarshal(data, &cloned))
	return &cloned
}

// 多会话测试中，握手头与 Body 必须来自同一个客户端输入，避免夹具混用标识。
func schedulerContinuityForward(t *testing.T, account *Account, repo AccountRepository, body []byte) [4]string {
	t.Helper()
	account.Credentials = map[string]any{"access_token": "synthetic-test-token"}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(bytes.NewBufferString("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "codex-tui/0.153.3 (Windows 10.0.26200; x86_64) xterm-256color")
	for _, field := range []string{"session", "thread"} {
		c.Request.Header.Set(field+"-id", gjson.GetBytes(body, "client_metadata."+field+"_id").String())
	}
	c.Request.Header.Set("x-codex-turn-metadata", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String())
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, gjson.GetBytes(body, "prompt_cache_key").String(), gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	return [4]string{
		upstream.lastReq.Header.Get("session-id"), upstream.lastReq.Header.Get("thread-id"),
		gjson.GetBytes(upstream.lastBody, "client_metadata.turn_id").String(),
		gjson.GetBytes(upstream.lastBody, "client_metadata.context_window_id").String(),
	}
}

// 同一账号的两个会话来回切换，再将相同客户端输入发给第二账号；分别检查稳定和隔离。
func TestCodexSchedulerSessionAndAccountIsolation(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, sticky := range []bool{false, true} {
			t.Run(fmt.Sprintf("raw=%v/sticky=%v", raw, sticky), func(t *testing.T) {
				bodies := make(map[string][]byte)
				digests := make(map[string][32]byte)
				for _, session := range []string{"a", "b"} {
					base := codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000998", 1700000000000, 0, " key-"+session+" ")
					var body map[string]any
					require.NoError(t, json.Unmarshal(base, &body))
					meta := body["client_metadata"].(map[string]any)
					var nested map[string]any
					require.NoError(t, json.Unmarshal([]byte(meta["x-codex-turn-metadata"].(string)), &nested))
					for _, key := range []string{"session_id", "thread_id"} {
						meta[key] = "client-" + session + "-" + key
						nested[key] = meta[key]
					}
					nested["context_window_id"] = "client-" + session + "-window"
					encoded, err := json.Marshal(nested)
					require.NoError(t, err)
					meta["x-codex-turn-metadata"] = string(encoded)
					bodies[session], err = json.Marshal(body)
					require.NoError(t, err)
					digests[session] = sha256.Sum256(bodies[session])
				}
				baselines := make(map[string][4]string)
				for owner := 0; owner < 2; owner++ {
					account := newTestOAuthAccount(993000+codexSnapshotTestAccountID.Add(1), map[string]any{
						codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw,
					})
					account.Status, account.Schedulable, account.Concurrency = StatusActive, true, 1
					stale, durable := schedulerContinuityClone(t, account), schedulerContinuityClone(t, account)
					events := []string{}
					repo := &schedulerContinuityRepo{codexIdentityPersistenceRepo: codexIdentityPersistenceRepo{account: account, latest: durable}, t: t, events: &events}
					cache := &schedulerContinuityCache{snapshotHydrationCache: snapshotHydrationCache{
						snapshot: []*Account{stale}, accounts: map[int64]*Account{account.ID: stale},
					}, t: t, events: &events}
					gatewayCache := &schedulerTestGatewayCache{}
					svc := &OpenAIGatewayService{accountRepo: repo, cache: gatewayCache,
						schedulerSnapshot:  NewSchedulerSnapshotService(cache, nil, repo, nil, nil),
						concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
					}
					for step, session := range []string{"a", "b", "a", "b"} {
						schedulerContinuityColdAccount(account.ID, false)
						body := bodies[session]
						require.Equal(t, digests[session], sha256.Sum256(body), "客户端输入在重试间必须逐字节相同")
						hash := ""
						if sticky {
							hash = "probe-session-" + session
						}
						selection, _, err := svc.SelectAccountWithSchedulerForCapability(context.Background(), nil, "", hash, "gpt-6-astra", nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, false, true)
						require.NoError(t, err)
						require.NotNil(t, selection.Account)
						ids := schedulerContinuityForward(t, selection.Account, repo, body)
						selection.ReleaseFunc()
						key := fmt.Sprintf("%d/%s", owner, session)
						if previous, ok := baselines[key]; ok {
							require.Equal(t, previous, ids, "同账号、同客户端会话应复用原来的整组身份")
						} else {
							for _, other := range baselines {
								for i := range ids {
									require.NotEmpty(t, ids[i])
									require.NotEqual(t, other[i], ids[i], "不同会话或账号不得合并")
								}
							}
							baselines[key] = ids
						}
						t.Logf("owner=%d session=%s step=%d same_input_verified=true", owner, session, step)
					}
					schedulerContinuityColdAccount(account.ID, false)
				}
				require.Len(t, baselines, 4)
			})
		}
	}
}

// 来源标记仅活在当前选择对象中，缓存反序列化后必须重新走正常水合与资格检查。
func TestOpenAISchedulerVerifiedSnapshotMarkerIsRequestLocal(t *testing.T) {
	account := &Account{ID: 993099, schedulerDBVerified: true}
	cloned := schedulerContinuityClone(t, account)
	require.False(t, cloned.schedulerDBVerified)
	cache := &snapshotHydrationCache{accounts: map[int64]*Account{account.ID: {ID: account.ID, Name: "hydrated"}}}
	svc := &OpenAIGatewayService{schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}
	selected, err := svc.hydrateSelectedAccount(context.Background(), account)
	require.NoError(t, err)
	require.Same(t, account, selected)
	selected, err = svc.hydrateSelectedAccount(context.Background(), cloned)
	require.NoError(t, err)
	require.Equal(t, "hydrated", selected.Name)
}

// 记录真实调度读取的先后顺序，不记录账号凭据或身份原文。
type schedulerContinuityRepo struct {
	codexIdentityPersistenceRepo
	t      *testing.T
	events *[]string
	reads  int
}

func (r *schedulerContinuityRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	account, err := r.codexIdentityPersistenceRepo.GetByID(ctx, id)
	if err != nil || account == nil {
		return account, err
	}
	r.reads++
	*r.events = append(*r.events, fmt.Sprintf("db(bindings=%v)", len(readCodexIdentityBindings(account)) > 0))
	return schedulerContinuityClone(r.t, account), nil
}

type schedulerContinuityCache struct {
	snapshotHydrationCache
	t      *testing.T
	events *[]string
}

func (c *schedulerContinuityCache) GetAccount(ctx context.Context, id int64) (*Account, error) {
	account, err := c.snapshotHydrationCache.GetAccount(ctx, id)
	if err != nil || account == nil {
		return account, err
	}
	*c.events = append(*c.events, fmt.Sprintf("cache(bindings=%v)", len(readCodexIdentityBindings(account)) > 0))
	return schedulerContinuityClone(c.t, account), nil
}

// 只清理本场景的合成账号，避免影响已有回归测试的进程状态。
func schedulerContinuityColdAccount(id int64, expired bool) {
	prefix := fmt.Sprintf("%d:", id)
	codexIdentityHotCache.Range(func(key, value any) bool {
		text, ok := key.(string)
		if !ok || !strings.HasPrefix(text, prefix) {
			return true
		}
		if expired {
			hot := value.(codexIdentityHotBinding)
			hot.LastUsedAtMS = time.Now().Add(-codexIdentityHotCacheTTL - time.Hour).UnixMilli()
			codexIdentityHotCache.Store(key, hot)
		} else {
			codexIdentityHotCache.Delete(key)
		}
		return true
	})
	codexIdentityPersistedHashes.Delete(fmt.Sprint(id))
}

// 从公共调度入口走到 Forward；不直接把缺绑定快照传给 Forward。
func TestCodexSchedulerToForwardContinuity(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, mode := range []string{"cold_stale", "expired_stale", "warm_stale", "cold_fresh", "cold_db_control", "cold_advanced_control"} {
			t.Run(fmt.Sprintf("raw=%v/%s", raw, mode), func(t *testing.T) {
				account := newTestOAuthAccount(991000+codexSnapshotTestAccountID.Add(1), map[string]any{
					codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw,
				})
				account.Status, account.Schedulable, account.Concurrency = StatusActive, true, 1
				account.UpdatedAt = time.Now().Add(-time.Minute)
				ctx := context.Background()
				var groupID *int64
				if mode == "cold_advanced_control" {
					id := int64(998891)
					groupID = &id
					account.GroupIDs = []int64{id}
					ctx = withAdvancedSchedulerTestGroup(ctx, id)
				}
				stale := schedulerContinuityClone(t, account)
				durable := schedulerContinuityClone(t, account)
				events := []string{}
				repo := &schedulerContinuityRepo{codexIdentityPersistenceRepo: codexIdentityPersistenceRepo{account: account, latest: durable}, t: t, events: &events}
				body := codexSnapshotTestBody(t, "01993000-0000-7000-8000-000000000997", time.Now().UnixMilli(), 0, "deep-control-key")
				first := codexSnapshotTestForward(t, account, repo, body)
				durable.UpdatedAt = time.Now()
				require.NotEmpty(t, readCodexIdentityBindings(durable))
				// UUIDv7 turn 走确定性映射，不要求该场景生成非 UUIDv7 回合存储。
				t.Cleanup(func() { schedulerContinuityColdAccount(account.ID, false) })
				if mode != "warm_stale" {
					schedulerContinuityColdAccount(account.ID, mode == "expired_stale")
				}
				cached := stale
				if mode == "cold_fresh" {
					cached = schedulerContinuityClone(t, durable)
				}
				cache := &schedulerContinuityCache{
					snapshotHydrationCache: snapshotHydrationCache{snapshot: []*Account{stale}, accounts: map[int64]*Account{account.ID: cached}},
					t:                      t, events: &events,
				}
				svc := &OpenAIGatewayService{
					accountRepo:        repo,
					schedulerSnapshot:  NewSchedulerSnapshotService(cache, nil, repo, nil, nil),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
				}
				events, repo.reads = nil, 0
				selection, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, groupID, "", "", "gpt-6-astra", nil, OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityResponses, false, true)
				require.NoError(t, err)
				require.NotNil(t, selection)
				require.True(t, selection.Acquired)
				defer selection.ReleaseFunc()
				require.Positive(t, repo.reads, "本探针必须实际进入数据库复核")
				selectedHasBindings := len(readCodexIdentityBindings(selection.Account)) > 0
				selectedIsLatest := selection.Account.UpdatedAt.Equal(durable.UpdatedAt)
				t.Logf("mode=%s selection_trace=%s selected_has_bindings=%v selected_latest=%v", mode, strings.Join(events, " -> "), selectedHasBindings, selectedIsLatest)
				// 对照只改变最终快照来源；生成器、持久化合并及请求输入完全不变。
				if mode == "cold_db_control" {
					selection.Account, err = repo.GetByID(context.Background(), account.ID)
					require.NoError(t, err)
				}
				time.Sleep(3 * time.Millisecond)
				second := codexSnapshotTestForward(t, selection.Account, repo, body)
				sessionSame := first.lastReq.Header.Get("session-id") == second.lastReq.Header.Get("session-id")
				threadSame := first.lastReq.Header.Get("thread-id") == second.lastReq.Header.Get("thread-id")
				turnSame := gjson.GetBytes(first.lastBody, "client_metadata.turn_id").String() == gjson.GetBytes(second.lastBody, "client_metadata.turn_id").String()
				windowSame := gjson.GetBytes(first.lastBody, "client_metadata.context_window_id").String() == gjson.GetBytes(second.lastBody, "client_metadata.context_window_id").String()
				t.Logf("mode=%s session_stable=%v thread_stable=%v turn_stable=%v window_stable=%v", mode, sessionSame, threadSame, turnSame, windowSame)
				require.Equal(t, "deep-control-key", gjson.GetBytes(second.lastBody, "prompt_cache_key").String())
				require.Equal(t, first.lastReq.Header.Get("User-Agent"), second.lastReq.Header.Get("User-Agent"))
				require.True(t, sessionSame && threadSame && turnSame && windowSame, "经过数据库复核的真实调度链仍丢失既有绑定")
			})
		}
	}
}
