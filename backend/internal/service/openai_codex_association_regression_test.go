package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const auditAssocRoot = "01993000-0000-7000-8000-000000000201"
const auditAssocChild = "01993000-0000-7000-8000-000000000202"
const auditAssocTurn = "01993000-0000-7000-8000-000000000211"

// 使用官方字符串字典与内嵌元数据载体；只含本地合成数据。
func auditAssocBody(t *testing.T, session, thread, turn, window string, generation int, extra map[string]any) []byte {
	t.Helper()
	nested := map[string]any{
		"installation_id": "synthetic-installation", "session_id": session,
		"thread_id": thread, "turn_id": turn, "window_id": fmt.Sprintf("%s:%d", thread, generation),
		"window_number": generation, "context_window_id": window,
		"request_kind": "turn", "turn_started_at_unix_ms": int64(1789100000000),
	}
	for key, value := range extra {
		if value == nil {
			delete(nested, key)
		} else {
			nested[key] = value
		}
	}
	encoded, err := json.Marshal(nested)
	require.NoError(t, err)
	metadata := map[string]any{
		"x-codex-installation-id": "synthetic-installation", "session_id": session,
		"thread_id": thread, "turn_id": turn,
		"x-codex-window-id":     fmt.Sprintf("%s:%d", thread, generation),
		"x-codex-turn-metadata": string(encoded),
	}
	if parent, ok := extra["parent_thread_id"].(string); ok {
		metadata["x-codex-parent-thread-id"] = parent
	}
	for _, key := range []string{"parent_turn_id", "root_turn_id"} {
		if value, ok := extra[key].(string); ok {
			metadata[key] = value
		}
	}
	result, err := json.Marshal(map[string]any{
		"model": "gpt-6-astra", "instructions": "local synthetic protocol audit",
		"input": []any{}, "stream": false, "prompt_cache_key": "unchanged-synthetic-key",
		"client_metadata": metadata,
	})
	require.NoError(t, err)
	return result
}

// 覆盖真实普通/透传 Forward 的最终发送边界，不访问网络。
func auditAssocForward(t *testing.T, account *Account, repo AccountRepository, body []byte) *httpUpstreamRecorder {
	t.Helper()
	account.Credentials = map[string]any{"access_token": "synthetic-test-token"}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(bytes.NewBufferString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_synthetic\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
	}}
	svc := &OpenAIGatewayService{accountRepo: repo, httpUpstream: upstream}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "codex-tui/0.153.3 (Windows 10.0.26200; x86_64) xterm-256color")
	for header, field := range map[string]string{
		"session-id": "session_id", "thread-id": "thread_id",
		"x-codex-turn-metadata":    "x-codex-turn-metadata",
		"x-codex-parent-thread-id": "x-codex-parent-thread-id",
		"x-codex-window-id":        "x-codex-window-id",
	} {
		if value := gjson.GetBytes(body, "client_metadata."+field).String(); value != "" {
			c.Request.Header.Set(header, value)
		}
	}
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	return upstream
}

// 对照同一根会话内的父子图，以及独立根会话不会合并的控制组。
func TestAuditAssocParentGraphControl(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			account := newTestOAuthAccount(998000+codexSnapshotTestAccountID.Add(1),
				map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw})
			repo := &codexIdentityPersistenceRepo{account: account}
			parentBody := auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, "01993000-0000-7000-8000-000000000221", 0, map[string]any{"root_turn_id": auditAssocTurn})
			parent := auditAssocForward(t, account, repo, parentBody)
			childBody := auditAssocBody(t, auditAssocRoot, auditAssocChild, "01993000-0000-7000-8000-000000000212", "01993000-0000-7000-8000-000000000222", 0,
				map[string]any{"parent_thread_id": auditAssocRoot, "parent_turn_id": auditAssocTurn, "root_turn_id": auditAssocTurn})
			child := auditAssocForward(t, account, repo, childBody)
			parentTurn := gjson.GetBytes(parent.lastBody, "client_metadata.turn_id").String()
			childMeta := gjson.GetBytes(child.lastBody, "client_metadata.x-codex-turn-metadata").String()
			require.Equal(t, parent.lastReq.Header.Get("session-id"), child.lastReq.Header.Get("session-id"))
			require.NotEqual(t, parent.lastReq.Header.Get("thread-id"), child.lastReq.Header.Get("thread-id"))
			require.Equal(t, parent.lastReq.Header.Get("thread-id"), gjson.Get(childMeta, "parent_thread_id").String())
			require.Equal(t, parentTurn, gjson.Get(childMeta, "parent_turn_id").String())
			require.Equal(t, parentTurn, gjson.Get(childMeta, "root_turn_id").String())
			retry := auditAssocForward(t, account, repo, parentBody)
			require.Equal(t, parentTurn, gjson.GetBytes(retry.lastBody, "client_metadata.turn_id").String())
			otherBody := auditAssocBody(t, auditAssocChild, auditAssocChild, "01993000-0000-7000-8000-000000000213", "01993000-0000-7000-8000-000000000223", 0, nil)
			other := auditAssocForward(t, account, repo, otherBody)
			require.NotEqual(t, parent.lastReq.Header.Get("session-id"), other.lastReq.Header.Get("session-id"))
			t.Log("ASSOC_CONTROL parent_thread=true parent_turn=true root_turn=true same_turn=true independent_sessions=true")
		})
	}
}

// 官方回退可恢复早期窗口，再压缩会生成新 UUID；相同代数不代表同一个窗口实例。
func TestAuditAssocWindowRollbackIdentity(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			account := newTestOAuthAccount(998000+codexSnapshotTestAccountID.Add(1),
				map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw})
			repo := &codexIdentityPersistenceRepo{account: account}
			windows := []string{"01993000-0000-7000-8000-000000000221", "01993000-0000-7000-8000-000000000222", "01993000-0000-7000-8000-000000000221", "01993000-0000-7000-8000-000000000223"}
			generations := []int{0, 1, 0, 1}
			var mapped []string
			for i, window := range windows {
				captured := auditAssocForward(t, account, repo, auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, window, generations[i], nil))
				meta := gjson.GetBytes(captured.lastBody, "client_metadata.x-codex-turn-metadata").String()
				require.Equal(t, int64(generations[i]), gjson.Get(meta, "window_number").Int())
				require.Equal(t, "unchanged-synthetic-key", gjson.GetBytes(captured.lastBody, "prompt_cache_key").String())
				mapped = append(mapped, gjson.Get(meta, "context_window_id").String())
			}
			require.Equal(t, mapped[0], mapped[2], "回退到已知旧窗口应复用原绑定")
			require.NotEqual(t, mapped[0], mapped[1], "正常压缩应切换窗口")
			require.NotEqual(t, mapped[1], mapped[3], "回退后重新压缩产生的不同窗口被同一代数合并")
		})
	}
}

// 边界契约：首笔未带时间时的服务端兜底，不应被误认成客户端首次有效时间。
func TestAuditAssocFirstValidTimestamp(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			account := newTestOAuthAccount(998000+codexSnapshotTestAccountID.Add(1),
				map[string]any{codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": raw})
			repo := &codexIdentityPersistenceRepo{account: account}
			first := auditAssocForward(t, account, repo,
				auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, "01993000-0000-7000-8000-000000000221", 0, map[string]any{"turn_started_at_unix_ms": nil}))
			second := auditAssocForward(t, account, repo,
				auditAssocBody(t, auditAssocRoot, auditAssocRoot, auditAssocTurn, "01993000-0000-7000-8000-000000000221", 0, nil))
			firstTime := gjson.Get(first.lastReq.Header.Get("x-codex-turn-metadata"), "turn_started_at_unix_ms").Int()
			secondTime := gjson.Get(second.lastReq.Header.Get("x-codex-turn-metadata"), "turn_started_at_unix_ms").Int()
			t.Logf("FIRST_VALID_TIME server_fallback_reused=%v client_first_valid_used=%v", firstTime == secondTime, secondTime == 1789100000000)
			require.Equal(t, int64(1789100000000), secondTime, "首次有效客户端时间被之前的兜底缓存遮蔽")
		})
	}
}
