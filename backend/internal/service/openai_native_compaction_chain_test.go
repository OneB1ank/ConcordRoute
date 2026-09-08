package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 所有上游响应均为合成数据；这里没有网络透传实现，也不使用真实凭据。
type nativeCompactMock struct {
	mu      sync.Mutex
	auto    bool
	inputs  [][]byte
	outputs [][]byte
	paths   []string
}

func (m *nativeCompactMock) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inputs = append(m.inputs, raw)
	m.paths = append(m.paths, req.URL.Path)
	id := fmt.Sprintf("resp_synthetic_%d", len(m.inputs))
	compact := strings.HasSuffix(req.URL.Path, "/compact")
	trigger := false
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		trigger = trigger || item.Get("type").String() == "compaction_trigger"
	}
	output := []any{map[string]any{
		"id": id + "_msg", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "SYNTHETIC_REPLY", "annotations": []any{}}},
	}}
	if compact || trigger {
		output = []any{}
		if compact {
			// 旧式 compact 返回完整替换窗口；保留用户条目后追加不透明状态。
			for _, item := range gjson.GetBytes(raw, "input").Array() {
				if item.Get("role").String() == "user" {
					output = append(output, json.RawMessage(item.Raw))
				}
			}
		}
		output = append(output, map[string]any{
			"id": "cmp_synthetic", "type": "compaction",
			"encrypted_content": "SYNTHETIC_OPAQUE_STATE_+/=_不得改写",
		})
	}
	inputTokens := 1000
	if m.auto && len(m.inputs) == 2 {
		// 合成大用量仅用于触发客户端自动压缩，不代表真实模型计算量。
		inputTokens = 330000
	}
	response := map[string]any{
		"id": id, "object": "response", "status": "completed",
		"model": gjson.GetBytes(raw, "model").String(), "output": output,
		// 故意不模拟任何缓存收益，避免把本地夹具当作真实命中率证据。
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": 1, "total_tokens": inputTokens + 1,
			"input_tokens_details": map[string]any{"cached_tokens": 0},
		},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, err
	}
	m.outputs = append(m.outputs, encoded)
	contentType := "application/json"
	body := string(encoded)
	if !compact {
		contentType = "text/event-stream"
		var stream strings.Builder
		for i, item := range output {
			event, marshalErr := json.Marshal(map[string]any{
				"type": "response.output_item.done", "output_index": i, "item": item,
			})
			if marshalErr != nil {
				return nil, marshalErr
			}
			fmt.Fprintf(&stream, "data: %s\n\n", event)
		}
		fmt.Fprintf(&stream, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", encoded)
		body = stream.String()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{contentType},
			"X-Request-Id": []string{id},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (m *nativeCompactMock) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return m.Do(req, proxy, id, concurrency)
}

// 普通转换有既定的 store=false 输入 ID 清理；比较正文时只豁免该已知差异。
// 顺序、角色、正文、不透明压缩内容与所有其他字段仍做完整相等断言。
func requireNativeCompactInput(t *testing.T, original, sent []byte, passthrough bool) {
	t.Helper()
	expected := gjson.GetBytes(original, "input").Raw
	actual := gjson.GetBytes(sent, "input").Raw
	if !passthrough {
		var items []map[string]any
		var actualItems []map[string]any
		require.NoError(t, json.Unmarshal([]byte(expected), &items))
		require.NoError(t, json.Unmarshal([]byte(actual), &actualItems))
		require.Len(t, actualItems, len(items))
		for i, item := range items {
			if _, retained := actualItems[i]["id"]; !retained {
				delete(item, "id")
			}
		}
		normalized, err := json.Marshal(items)
		require.NoError(t, err)
		expected = string(normalized)
	}
	require.JSONEq(t, expected, actual)
}

// 直接调用真实 Forward：先生成，再压缩，最后使用返回的窗口续聊。
// 显式客户端键允许切换；本测试不会要求压缩前后固定同一个键。
func TestCodexNativeCompactionForwardChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, strategy := range []string{"legacy", "v2"} {
			t.Run(fmt.Sprintf("passthrough=%t/%s", passthrough, strategy), func(t *testing.T) {
				account := newTestOAuthAccount(18601, map[string]any{
					codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
				})
				account.Credentials = map[string]any{"access_token": "synthetic-only"}
				account.Concurrency = 1
				upstream := &nativeCompactMock{}
				svc := &OpenAIGatewayService{httpUpstream: upstream}
				session := uuid.NewString()
				base := map[string]any{
					"model": "gpt-6-astra", "stream": true,
					"instructions": "固定的合成系统指令",
					"tools": []any{map[string]any{
						"type": "function", "name": "synthetic_tool", "description": "不执行",
						"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
					}},
					"input": []any{map[string]any{
						"type": "message", "role": "user", "content": []any{map[string]any{
							"type": "input_text", "text": strings.Repeat("固定用户前缀 ", 512),
						}},
					}},
					"prompt_cache_key": "cache-A",
					"client_metadata":  map[string]any{"window_number": "0"},
				}
				forward := func(path string, body map[string]any) []byte {
					t.Helper()
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
					c.Request.Header.Set("session_id", session)
					result, err := svc.Forward(context.Background(), c, account, raw)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, http.StatusOK, rec.Code)
					sent := upstream.inputs[len(upstream.inputs)-1]
					requireNativeCompactInput(t, raw, sent, passthrough)
					require.JSONEq(t, gjson.GetBytes(raw, "tools").Raw, gjson.GetBytes(sent, "tools").Raw)
					require.Equal(t, body["instructions"], gjson.GetBytes(sent, "instructions").String())
					require.False(t, gjson.GetBytes(sent, "root_turn_id").Exists())
					require.False(t, gjson.GetBytes(sent, "client_metadata.root_turn_id").Exists())
					return rec.Body.Bytes()
				}
				first := forward("/v1/responses", base)
				require.Contains(t, string(first), "response.completed")
				require.Equal(t, "cache-A", gjson.GetBytes(upstream.inputs[0], "prompt_cache_key").String())
				history, ok := base["input"].([]any)
				require.True(t, ok)
				normalOutput := gjson.GetBytes(upstream.outputs[0], "output")
				for _, item := range normalOutput.Array() {
					history = append(history, json.RawMessage(item.Raw))
				}
				base["input"] = history
				compactPath := "/v1/responses/compact"
				delete(base, "stream")
				if strategy == "v2" {
					compactPath = "/v1/responses"
					base["stream"] = true
					base["input"] = append(append([]any{}, history...), map[string]any{"type": "compaction_trigger"})
				}
				compactReply := forward(compactPath, base)
				require.Contains(t, string(compactReply), "SYNTHETIC_OPAQUE_STATE")
				require.Equal(t, "cache-A", gjson.GetBytes(upstream.inputs[1], "prompt_cache_key").String(),
					"压缩请求也应保留客户端明确提供的缓存键")
				compacted := gjson.GetBytes(upstream.outputs[1], "output").Array()
				nextInput := []any{}
				if strategy == "v2" {
					nextInput = append(nextInput, history[0])
				}
				for _, item := range compacted {
					nextInput = append(nextInput, json.RawMessage(item.Raw))
				}
				nextInput = append(nextInput, map[string]any{"type": "message", "role": "user", "content": "压缩后继续"})
				base["input"] = nextInput
				base["stream"] = true
				base["client_metadata"] = map[string]any{"window_number": "1"}
				base["prompt_cache_key"] = "cache-B"
				forward("/v1/responses", base)
				require.Equal(t, "cache-B", gjson.GetBytes(upstream.inputs[2], "prompt_cache_key").String())
				require.Equal(t, "SYNTHETIC_OPAQUE_STATE_+/=_不得改写", gjson.GetBytes(upstream.inputs[2], "input.#(type==\"compaction\").encrypted_content").String())
				delete(base, "prompt_cache_key")
				forward("/v1/responses", base)
				require.Equal(t, "cache-B", gjson.GetBytes(upstream.inputs[3], "prompt_cache_key").String())
				base["prompt_cache_key"] = "cache-C"
				forward("/v1/responses", base)
				require.Equal(t, "cache-C", gjson.GetBytes(upstream.inputs[4], "prompt_cache_key").String())
			})
		}
	}
}

// compact 显式键只透传，不将缺省值从普通请求补齐，也不污染其暂存绑定。
func TestCodexNativeCompactionCacheKeyBoundary(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			account := newTestOAuthAccount(18603, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
			account.Type = accountType
			account.Credentials = map[string]any{"access_token": "synthetic-only", "api_key": "synthetic-only"}
			account.Concurrency = 1
			session := uuid.NewString()
			upstream := &nativeCompactMock{}
			svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
			for _, key := range []string{`"explicit-compact-key"`, `null`, `""`, ``} {
				body := `{"model":"gpt-6-astra","instructions":"synthetic","input":[]}`
				if key != "" {
					body = strings.TrimSuffix(body, "}") + `,"prompt_cache_key":` + key + `}`
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
				c.Request.Header.Set("session_id", session)
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
				_, err := svc.Forward(context.Background(), c, account, []byte(body))
				require.NoError(t, err)
				require.Equal(t, key, gjson.GetBytes(upstream.inputs[len(upstream.inputs)-1], "prompt_cache_key").Raw)
			}
		})
	}
	// 使用真实 ID 解析器预置普通绑定；compact-only 显式键不会影响下一笔缺省请求。
	account := newTestOAuthAccount(18604, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	account.Credentials = map[string]any{"access_token": "synthetic-only"}
	account.Concurrency = 1
	headers := make(http.Header)
	headers.Set("session_id", uuid.NewString())
	resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{"prompt_cache_key":"normal-cache"}`))
	upstream := &nativeCompactMock{}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	body := []byte(`{"model":"gpt-6-astra","instructions":"synthetic","input":[],"prompt_cache_key":"compact-only-cache"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	c.Request.Header = headers.Clone()
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, "compact-only-cache", gjson.GetBytes(upstream.inputs[0], "prompt_cache_key").String())
	next := resolveCodexFingerprintIDsFromRawRequest(account, headers, []byte(`{}`))
	require.Equal(t, "normal-cache", next.promptCacheKey)
}

// 可选真实客户端闭环：独立 CODEX_HOME + app-server + 本地 Forward + 合成上游。
// 不启动真实工具，不访问模型服务，不修改运行中的客户端或服务器配置。
func TestCodexNativeCompactionClientLoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	binary := os.Getenv("CODEX_REMOTE_COMPACT_TEST_BINARY")
	python := os.Getenv("CODEX_REMOTE_COMPACT_TEST_PYTHON")
	if binary == "" || python == "" {
		t.Skip("需要显式提供隔离测试用 Codex 与 Python 可执行文件")
	}
	for _, strategy := range []string{
		"summary", "legacy", "v2", "summary-auto", "legacy-auto", "v2-auto",
		"summary-long", "legacy-long", "v2-long",
	} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/passthrough=%t", strategy, passthrough), func(t *testing.T) {
				dir := t.TempDir()
				if root := os.Getenv("CODEX_REMOTE_COMPACT_TEST_OUTPUT"); root != "" {
					dir = filepath.Join(root, fmt.Sprintf("%s-%t", strategy, passthrough))
					require.NoError(t, os.MkdirAll(dir, 0700))
				}
				upstream := &nativeCompactMock{auto: strings.HasSuffix(strategy, "-auto")}
				svc := &OpenAIGatewayService{httpUpstream: upstream}
				account := newTestOAuthAccount(18602, map[string]any{
					codexFingerprintModeExtraKey: "cockpit", "openai_passthrough": passthrough,
				})
				account.Credentials = map[string]any{"access_token": "synthetic-only"}
				account.Concurrency = 1
				var mu, handlerMu sync.Mutex
				var observations []map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// 将出站捕获与该请求的响应归档成一条记录，防止客户端紧接着发下一笔时交错。
					handlerMu.Lock()
					defer handlerMu.Unlock()
					// 只暴露模拟 Responses；其他路径明确失败，避免静默访问真实服务。
					if r.Method != http.MethodPost || (r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/responses/compact") {
						http.Error(w, "synthetic endpoint only", http.StatusNotFound)
						return
					}
					raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
					if err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					c, _ := gin.CreateTestContext(w)
					c.Request = r
					_, forwardErr := svc.Forward(r.Context(), c, account, raw)
					upstream.mu.Lock()
					var sent []byte
					if len(upstream.inputs) > 0 {
						sent = append([]byte{}, upstream.inputs[len(upstream.inputs)-1]...)
					}
					upstream.mu.Unlock()
					entry := map[string]any{
						"path": r.URL.Path, "client": json.RawMessage(raw), "upstream": json.RawMessage(sent),
					}
					if forwardErr != nil {
						entry["error"] = forwardErr.Error()
					}
					mu.Lock()
					observations = append(observations, entry)
					mu.Unlock()
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer cancel()
				args := []string{filepath.Join("testdata", "codex_native_compaction_client.py"),
					"--binary", binary, "--url", server.URL + "/v1", "--mode", strings.Split(strategy, "-")[0], "--out", dir}
				if strings.HasSuffix(strategy, "-auto") {
					args = append(args, "--auto")
				}
				if strings.HasSuffix(strategy, "-long") {
					args = append(args, "--history-repetitions", "12000")
				}
				cmd := exec.CommandContext(ctx, python, args...)
				output, runErr := cmd.CombinedOutput()
				require.NoError(t, os.WriteFile(filepath.Join(dir, "driver.log"), output, 0600))
				mu.Lock()
				evidence, marshalErr := json.MarshalIndent(observations, "", "  ")
				mu.Unlock()
				require.NoError(t, marshalErr)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "synthetic-wire.json"), evidence, 0600))
				require.NoError(t, runErr, "%s", output)
				t.Logf("%s", output)
				require.Len(t, observations, 5, "两次普通请求、一次压缩、两次续聊")
				for _, entry := range observations {
					require.NotContains(t, entry, "error")
					raw, ok := entry["client"].(json.RawMessage)
					require.True(t, ok)
					sent, ok := entry["upstream"].(json.RawMessage)
					require.True(t, ok)
					requireNativeCompactInput(t, raw, sent, passthrough)
					require.Equal(t, gjson.GetBytes(raw, "prompt_cache_key").String(), gjson.GetBytes(sent, "prompt_cache_key").String())
				}
				last, ok := observations[3]["upstream"].(json.RawMessage)
				require.True(t, ok)
				if !strings.HasPrefix(strategy, "summary") {
					require.Equal(t, "SYNTHETIC_OPAQUE_STATE_+/=_不得改写", gjson.GetBytes(last, "input.#(type==\"compaction\").encrypted_content").String())
				} else {
					require.False(t, gjson.GetBytes(last, "input.#(type==\"compaction\")").Exists())
				}
			})
		}
	}
}
