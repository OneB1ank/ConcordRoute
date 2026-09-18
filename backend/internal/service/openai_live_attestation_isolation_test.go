package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// 已选择的协商通道失效时，禁止改用直传头或平台提供器，避免更换证明来源。
func TestLiveAttestationBoundContextDoesNotChangeSource(t *testing.T) {
	for _, bound := range []struct {
		name      string
		accountID int64
		envelope  string
	}{
		{"empty", 42, ""},
		{"malformed", 42, `{"v":1,"s":0,"t":""}`},
		{"wrong_account", 43, `{"v":1,"s":0,"t":"v1.test-other-account"}`},
	} {
		for _, source := range []string{"direct_header", "provider", "none"} {
			t.Run(bound.name+"/"+source, func(t *testing.T) {
				service := &OpenAIGatewayService{
					liveAttestationCipher: newLiveAttestationCipher(&config.Config{
						JWT: config.JWTConfig{Secret: "test-live-bound-source"},
					}),
				}
				var identity LiveCallIdentity
				if source == "direct_header" {
					identity = LiveCallIdentity{
						UserAgent:                 "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)",
						Originator:                "Codex Desktop",
						ClientAttestationEnvelope: `{"v":1,"s":0,"t":"v1.test-direct"}`,
					}
				}
				if source == "provider" {
					service.liveAttestation = liveAttestationStub{header: `{"v":1,"s":0,"t":"v1.test-provider"}`}
				}
				ctx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
					Key: liveattestation.SessionKey{
						AccountID: bound.accountID, ConnectionID: "test-connection",
						SessionID: "session-a", ThreadID: "thread-a",
					},
					Envelope: bound.envelope,
				})
				header, ciphertext, err := service.prepareLiveAttestationForRequest(ctx, &Account{
					ID: 42, Type: AccountTypeOAuth,
				}, identity)
				var unavailable *LiveAttestationUnavailableError
				require.ErrorAs(t, err, &unavailable)
				require.Empty(t, header)
				require.Empty(t, ciphertext)
			})
		}
	}
}

// 已限定 session 与 thread 的桥，两项都要匹配；缺一个不等于通配。
func TestCodexAppServerBridgeRegistryRequiresCompleteScope(t *testing.T) {
	registry := newCodexAppServerBridgeRegistry()
	bridge := &codexAppServerBridge{
		apiKeyID: 7, connectionID: "scoped", sessionID: "session-a", threadID: "thread-a",
		closed: make(chan struct{}),
	}
	bridge.negotiated.Store(true)
	bridge.initialized.Store(true)
	require.NoError(t, registry.add(bridge))
	t.Cleanup(func() { registry.remove(bridge) })
	for _, hints := range []struct {
		name    string
		session string
		thread  string
		want    bool
	}{
		{"both_match", "session-a", "thread-a", true},
		{"missing_thread", "session-a", "", false},
		{"missing_session", "", "thread-a", false},
		{"wrong_thread", "session-a", "thread-b", false},
		{"wrong_session", "session-b", "thread-a", false},
		{"neither", "", "", false},
	} {
		t.Run(hints.name, func(t *testing.T) {
			got, err := registry.find(7, hints.session, hints.thread)
			if hints.want {
				require.NoError(t, err)
				require.Same(t, bridge, got)
				return
			}
			require.ErrorIs(t, err, ErrCodexAppServerBridgeAmbiguous)
			require.Nil(t, got)
		})
	}
}

// 协议失败状态是有效 envelope，不得被直传头或提供器伪装成成功证明。
func TestLiveAttestationRelaysNegotiatedFailureStatus(t *testing.T) {
	service := &OpenAIGatewayService{
		liveAttestationCipher: newLiveAttestationCipher(&config.Config{
			JWT: config.JWTConfig{Secret: "test-live-failure-status"},
		}),
		liveAttestation: liveAttestationStub{header: `{"v":1,"s":0,"t":"v1.test-provider"}`},
	}
	for status := 1; status <= 4; status++ {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			envelope := fmt.Sprintf(`{"v":1,"s":%d}`, status)
			ctx := WithCodexAttestationRequestContext(context.Background(), CodexAttestationRequestContext{
				Key:      liveattestation.SessionKey{AccountID: 42, ConnectionID: "test-connection"},
				Envelope: envelope,
			})
			header, ciphertext, err := service.prepareLiveAttestationForRequest(ctx,
				&Account{ID: 42, Type: AccountTypeOAuth}, LiveCallIdentity{
					UserAgent:                 "Codex Desktop/0.153.4 (Windows 10.0.26200; x86_64)",
					Originator:                "Codex Desktop",
					ClientAttestationEnvelope: `{"v":1,"s":0,"t":"v1.test-direct"}`,
				})
			require.NoError(t, err)
			require.Equal(t, envelope, header)
			stored, err := service.decryptLiveAttestation(&LiveCallRecord{AttestationCiphertext: ciphertext})
			require.NoError(t, err)
			require.Equal(t, envelope, stored)
		})
	}
}

// 本地协议测试贯通真实 WebSocket 桥及 Live 出站构造；测试 token 仅发给假上游。
// 它验证每次创建重新取证明、Sideband 使用所属通话快照，不代表真实设备验收。
func TestLiveAttestationBridgeToCreateAndSideband(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	upstream := &liveHTTPUpstreamStub{}
	profileService, routerService := newLiveTLSRoutingServices()
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	capture, err := collector.CreateSession()
	require.NoError(t, err)
	service := &OpenAIGatewayService{
		cfg:                       &config.Config{},
		httpUpstream:              upstream,
		tlsFPProfileService:       profileService,
		tlsFPRouterService:        routerService,
		codexAttestationStore:     liveattestation.NewAppServerAttestationStore(time.Minute),
		codexAppServerBridges:     newCodexAppServerBridgeRegistry(),
		codexAttestationCollector: collector,
		liveAttestationCipher: newLiveAttestationCipher(&config.Config{
			JWT: config.JWTConfig{Secret: "test-live-bridge-chain"},
		}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		_ = service.serveCodexAppServerBridge(ctx, 7, conn, "session-a", "thread-a", CodexAttestationHandshakeMetadata{}, capture.Token)
	}))
	defer server.Close()
	client, _, err := coderws.Dial(ctx, server.URL, nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(
		`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`)))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"method":"initialized"}`)))
	require.Eventually(t, func() bool {
		bridge, findErr := service.codexAppServerBridges.find(7, "session-a", "thread-a")
		return findErr == nil && bridge != nil
	}, time.Second, time.Millisecond)

	account := &Account{
		ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 2,
		Credentials: map[string]any{"access_token": "test-access-token", "chatgpt_account_id": "test-account"},
		Extra:       map[string]any{"enable_tls_fingerprint": true, "tls_fingerprint_router_id": int64(9)},
	}
	routing := service.matchLiveTLSFingerprintRouter(account, "test-live-client")
	var snapshots []string
	for n := 0; n < 2; n++ {
		type bindResult struct {
			ctx context.Context
			err error
		}
		done := make(chan bindResult, 1)
		go func() {
			bound, err := service.bindCodexAppServerAttestationContextForAPIKey(ctx, 7, account, "session-a", "thread-a")
			done <- bindResult{bound, err}
		}()
		_, message, err := client.Read(ctx)
		require.NoError(t, err)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		require.NoError(t, json.Unmarshal(message, &request))
		require.Equal(t, "attestation/generate", request.Method)
		token := fmt.Sprintf("v1.test-local-live-%d", n)
		response, err := json.Marshal(map[string]any{"id": request.ID, "result": map[string]string{"token": token}})
		require.NoError(t, err)
		require.NoError(t, client.Write(ctx, coderws.MessageText, response))
		var result bindResult
		select {
		case result = <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		require.NoError(t, result.err)
		header, ciphertext, err := service.prepareLiveAttestationForRequest(result.ctx, account, LiveCallIdentity{})
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf(`{"v":1,"s":0,"t":%q}`, token), header)
		require.NotContains(t, ciphertext, token)
		snapshots = append(snapshots, ciphertext)
		_, err = service.createUpstreamLiveCall(result.ctx, account, &LiveCallRequest{
			SDP: "v=offer\r\n", Session: json.RawMessage(`{"model":"gpt-live-1-codex"}`),
		}, header, routing)
		require.NoError(t, err)
		require.Equal(t, header, upstream.request.Header.Get(liveAttestationHeader))
		require.Equal(t, "live-routed", upstream.tlsProfile.Name)
		require.Equal(t, "codex_vscode/0.145.0 live-test", upstream.request.Header.Get("User-Agent"))
	}
	// 第二次生成后，第一通话仍使用自己的加密快照，不读取桥内后来生成的证明。
	for n, ciphertext := range snapshots {
		headers, err := service.liveSidebandHeaders(ctx, account, &LiveCallRecord{
			AccountID: 42, AttestationCiphertext: ciphertext,
		}, routing)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf(`{"v":1,"s":0,"t":"v1.test-local-live-%d"}`, n), headers.Get(liveAttestationHeader))
		require.Equal(t, "codex_vscode/0.145.0 live-test", headers.Get("User-Agent"))
	}
	records, err := collector.ListCaptures(capture.Token)
	require.NoError(t, err)
	require.Len(t, records, 3)
	for _, record := range records[:2] {
		require.Equal(t, "generate", record.Event)
		require.Equal(t, "success", record.Status)
		require.Equal(t, len("v1.test-local-live-0"), record.ProofLength)
		require.Len(t, record.ProofSHA256, 64)
		require.Equal(t, int64(7), record.APIKeyID)
		require.Equal(t, int64(42), record.AccountID)
	}
	summary, err := json.Marshal(records)
	require.NoError(t, err)
	require.NotContains(t, string(summary), "v1.test-local-live-")
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	require.Eventually(t, func() bool {
		bridge, err := service.codexAppServerBridges.find(7, "session-a", "thread-a")
		return err == nil && bridge == nil
	}, time.Second, time.Millisecond)
}
