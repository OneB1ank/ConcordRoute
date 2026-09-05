package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 真实透传重试必须重放最终出站快照，仅移除失效的续链锚点，版本不得回退。
func TestCodexVersionPassthroughRecoveryKeepsFinalMetadata(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		t.Run(accountType, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.IngressPreviousResponseRecoveryEnabled = true
			upstream := newStagedPassthroughConn()
			svc := newPassthroughLifecycleService(cfg, upstream)
			account := passthroughLifecycleAccount()
			account.Type = accountType
			account.Credentials["user_agent"] = "Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.901.41123)"
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			expectedVersion := "0.145.0"
			if accountType == AccountTypeOAuth {
				expectedVersion = "0.153.3"
			}
			controlCtx, cancelControl := context.WithCancel(context.Background())
			defer cancelControl()
			server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, account)
			defer server.Close()
			body := []byte(`{"type":"response.create","model":"gpt-5.1","stream":false,"previous_response_id":"resp_version_missing","prompt_cache_key":"cache-a","client_metadata":{"codex_version":"0.145.0","session_id":"session-a","thread_id":"thread-a","window_id":"thread-a:2","turn_id":"turn-a","root_turn_id":"root-a","context_window_id":"window-a","x-codex-turn-metadata":"{\"codex_version\":\"0.145.0\",\"tool\":\"shell\",\"future\":9007199254740993}"},"input":[{"role":"user","content":"keep codex_version 0.145.0"}]}`)
			client := dialPassthroughLifecycleClientWithFirstMessage(t, server, body)
			defer func() { _ = client.CloseNow() }()

			// 首帧与后续轮次分别触发一次真实恢复路径，不依赖模拟 helper 调用。
			for turn := 0; turn < 2; turn++ {
				if turn > 0 {
					writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
					err := client.Write(writeCtx, coderws.MessageText, body)
					cancelWrite()
					require.NoError(t, err)
				}
				firstWrite := requirePassthroughUpstreamWrite(t, upstream, 2*time.Second)
				require.Equal(t, expectedVersion, gjson.GetBytes(firstWrite, "client_metadata.codex_version").String())
				require.Equal(t, expectedVersion, gjson.Get(gjson.GetBytes(firstWrite, "client_metadata.x-codex-turn-metadata").String(), "codex_version").String())
				require.Equal(t, "resp_version_missing", gjson.GetBytes(firstWrite, "previous_response_id").String())
				upstream.Send(`{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"missing"}}`)
				retryWrite := requirePassthroughUpstreamWrite(t, upstream, 2*time.Second)
				require.False(t, gjson.GetBytes(retryWrite, "previous_response_id").Exists())
				require.Equal(t, expectedVersion, gjson.GetBytes(retryWrite, "client_metadata.codex_version").String(), "恢复重试不应使用改写前的版本")
				expectedRetry, removed, err := dropPreviousResponseIDFromRawPayload(firstWrite)
				require.NoError(t, err)
				require.True(t, removed)
				require.Equal(t, string(expectedRetry), string(retryWrite), "除续链锚点外，缓存键、身份、元数据和输入必须逐字节保持一致")
				require.Equal(t, "cache-a", gjson.GetBytes(retryWrite, "prompt_cache_key").String())
				require.Equal(t, "keep codex_version 0.145.0", gjson.GetBytes(retryWrite, "input.0.content").String())

				responseID := fmt.Sprintf("resp_version_recovered_%d", turn)
				upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`, responseID))
				completed, err := readPassthroughLifecycleFrame(t, client, 2*time.Second)
				require.NoError(t, err)
				require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
				require.Equal(t, responseID, gjson.GetBytes(completed, "response.id").String())
			}
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case <-serverErr:
			case <-time.After(3 * time.Second):
				t.Fatal("透传版本回归服务未退出")
			}
		})
	}
}
