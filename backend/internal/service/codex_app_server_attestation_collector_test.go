package service

import (
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/platform/liveattestation"
	"github.com/stretchr/testify/require"
)

func TestCodexAttestationCollectorCapturesInitializeAndOpaqueDigest(t *testing.T) {
	collector := NewCodexAppServerAttestationCollector()
	status := collector.Start()
	require.True(t, status.Running)
	session, err := collector.CreateSession()
	require.NoError(t, err)
	require.NotEmpty(t, session.Token)
	require.Contains(t, session.BridgeQuery, session.Token)

	key := liveattestation.SessionKey{AccountID: 41, ConnectionID: "conn-a", SessionID: "session-a", ThreadID: "thread-a"}
	initialize := []byte(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex-tui","version":"0.153.4"},"capabilities":{"requestAttestation":true}}}`)
	require.NoError(t, collector.RecordInitializeForAPIKey(session.Token, 7, key, initialize))
	require.NoError(t, collector.RecordGenerateResponseForAPIKey(session.Token, 7, key, 11, []byte(`{"id":11,"result":{"headerValue":"v1.real-proof"}}`)))
	records, err := collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "generate", records[0].Event)
	require.Equal(t, "success", records[0].Status)
	require.Equal(t, len("v1.real-proof"), records[0].ProofLength)
	require.NotEmpty(t, records[0].ProofSHA256)
	require.NotContains(t, records[0].ProofSHA256, "v1.real-proof")
	require.Equal(t, "initialize", records[1].Event)
	require.Equal(t, int64(41), records[1].AccountID)
	require.Equal(t, "codex-tui", records[1].ClientName)
	require.Equal(t, "0.153.4", records[1].ClientVersion)
}

func TestCodexAttestationCollectorRejectsCrossConnectionAndExpires(t *testing.T) {
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	session, err := collector.CreateSession()
	require.NoError(t, err)
	key := liveattestation.SessionKey{AccountID: 41, ConnectionID: "conn-a"}
	require.NoError(t, collector.RecordInitialize(session.Token, key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`)))
	other := liveattestation.SessionKey{AccountID: 41, ConnectionID: "conn-b"}
	require.Error(t, collector.RecordGenerateResponse(session.Token, other, 1, []byte(`{"id":1,"result":{"headerValue":"v1.proof"}}`)))

	collector.mu.Lock()
	collector.sessions[session.Token].expiresAt = time.Now().Add(-time.Second)
	collector.mu.Unlock()
	_, err = collector.ListCaptures(session.Token)
	require.Error(t, err)
}

func TestCodexAttestationCollectorRecordsFailuresWithoutProof(t *testing.T) {
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	session, err := collector.CreateSession()
	require.NoError(t, err)
	key := liveattestation.SessionKey{AccountID: 41, ConnectionID: "conn-a"}
	require.NoError(t, collector.RecordInitialize(session.Token, key, []byte(`{"id":1,"method":"initialize","params":{"capabilities":{"requestAttestation":true}}}`)))
	require.NoError(t, collector.RecordGenerateFailure(session.Token, key, 9, liveattestation.AttestationStatusTimeout))
	records, err := collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "timeout", records[0].Status)
	require.Zero(t, records[0].ProofLength)
	require.Empty(t, records[0].ProofSHA256)
}

func TestCodexAttestationCollectorCapturesProtocolAndHandshakeSummary(t *testing.T) {
	collector := NewCodexAppServerAttestationCollector()
	collector.Start()
	session, err := collector.CreateSession()
	require.NoError(t, err)
	key := liveattestation.SessionKey{AccountID: 41, ConnectionID: "conn-summary", SessionID: "session-a", ThreadID: "thread-a"}
	initialize := []byte(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"clientInfo":{"name":"codex-tui","version":"0.153.4"},"capabilities":{"requestAttestation":true,"experimentalApi":true}}}`)
	metadata := CodexAttestationHandshakeMetadata{
		HTTPProtocol: "HTTP/1.1",
		Transport:    "websocket",
		UserAgent:    "codex-tui/0.153.4 (Windows)",
		Originator:   "codex_cli_rs",
	}
	require.NoError(t, collector.RecordInitializeForAPIKeyWithMetadata(session.Token, 7, key, initialize, metadata))
	records, err := collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "2.0", records[0].JSONRPCVersion)
	require.Equal(t, []string{"experimentalApi", "requestAttestation"}, records[0].CapabilityKeys)
	require.NotEmpty(t, records[0].FrameSHA256)
	require.Equal(t, "HTTP/1.1", records[0].HandshakeProtocol)
	require.Equal(t, "websocket", records[0].HandshakeTransport)
	require.Equal(t, metadata.UserAgent, records[0].HandshakeUserAgent)
	require.Equal(t, metadata.Originator, records[0].HandshakeOriginator)

	require.NoError(t, collector.RecordGenerateResponseForAPIKey(session.Token, 7, key, 11, []byte(`{"id":11,"result":{"headerValue":"v1.real-proof"}}`)))
	records, err = collector.ListCaptures(session.Token)
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "11", records[0].GenerateResponseID)
	require.NotEmpty(t, records[0].FrameSHA256)
	require.Equal(t, records[0].HandshakeProtocol, records[1].HandshakeProtocol)
}
