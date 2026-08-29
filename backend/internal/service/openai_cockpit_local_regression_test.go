package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type localCockpitIdentitySnapshot struct {
	ids     *codexFingerprintIDs
	headers http.Header
	body    map[string]any
}

func newLocalCockpitRequest(threadID, cacheKey, sessionID string) (http.Header, map[string]any) {
	turnMetadata, _ := json.Marshal(map[string]any{
		"installation_id":  "client-installation",
		"session_id":       sessionID,
		"thread_id":        threadID,
		"turn_id":          "client-turn",
		"window_id":        threadID + ":0",
		"prompt_cache_key": cacheKey,
		"sandbox":          "seccomp",
	})
	headers := make(http.Header)
	headers.Set("session-id", sessionID)
	headers.Set("conversation_id", cacheKey)
	headers.Set("x-codex-installation-id", "client-installation")
	headers.Set("x-codex-window-id", threadID+":0")
	headers.Set("x-client-request-id", threadID)
	headers.Set("x-codex-turn-metadata", string(turnMetadata))
	body := map[string]any{
		"model":            "gpt-5.6-sol",
		"prompt_cache_key": cacheKey,
		"client_metadata": map[string]any{
			"x-codex-installation-id": "client-installation",
			"session_id":              sessionID,
			"thread_id":               threadID,
			"turn_id":                 "client-turn",
			"x-codex-window-id":       threadID + ":0",
			"x-codex-turn-metadata":   string(turnMetadata),
		},
	}
	return headers, body
}

func captureLocalCockpitIdentity(
	t *testing.T,
	account *Account,
	threadID, cacheKey, sessionID string,
	ids *codexFingerprintIDs,
) localCockpitIdentitySnapshot {
	t.Helper()
	headers, body := newLocalCockpitRequest(threadID, cacheKey, sessionID)
	if ids == nil {
		ids = bindCodexFingerprintIDsToAccount(
			resolveCodexFingerprintIDsFromRequest(account, headers, body),
			account,
		)
	}
	require.NotNil(t, ids)
	applyCodexFingerprintHeaders(headers, ids)
	require.True(t, applyCodexFingerprintClientMetadata(body, ids))
	return localCockpitIdentitySnapshot{ids: ids, headers: headers, body: body}
}

func requireLocalCockpitCarriersMatch(t *testing.T, snapshot localCockpitIdentitySnapshot) {
	t.Helper()
	ids := snapshot.ids
	require.Equal(t, ids.installationID, snapshot.headers.Get("x-codex-installation-id"))
	require.Equal(t, ids.sessionID, snapshot.headers.Get("session-id"))
	require.Equal(t, ids.sessionID, snapshot.headers.Get("session_id"))
	require.Equal(t, ids.threadID, snapshot.headers.Get("thread-id"))
	require.Equal(t, ids.threadID, snapshot.headers.Get("x-client-request-id"))
	require.Equal(t, ids.windowID, snapshot.headers.Get("x-codex-window-id"))
	require.Equal(t, ids.promptCacheKey, snapshot.headers.Get("conversation_id"))

	var headerMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(snapshot.headers.Get("x-codex-turn-metadata")), &headerMetadata))
	require.Equal(t, ids.installationID, headerMetadata["installation_id"])
	require.Equal(t, ids.sessionID, headerMetadata["session_id"])
	require.Equal(t, ids.threadID, headerMetadata["thread_id"])
	require.Equal(t, ids.turnID, headerMetadata["turn_id"])
	require.Equal(t, ids.windowID, headerMetadata["window_id"])
	require.Equal(t, ids.promptCacheKey, headerMetadata["prompt_cache_key"])
	require.Equal(t, "seccomp", headerMetadata["sandbox"])

	metadata, ok := snapshot.body["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, ids.installationID, metadata["x-codex-installation-id"])
	require.Equal(t, ids.sessionID, metadata["session_id"])
	require.Equal(t, ids.threadID, metadata["thread_id"])
	require.Equal(t, ids.turnID, metadata["turn_id"])
	require.Equal(t, ids.windowID, metadata["x-codex-window-id"])
	require.Equal(t, ids.promptCacheKey, snapshot.body["prompt_cache_key"])

	var bodyMetadata map[string]any
	embedded, ok := metadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(embedded), &bodyMetadata))
	require.Equal(t, headerMetadata, bodyMetadata)
}

func TestLocalCockpitIdentityTopologyAndFailover(t *testing.T) {
	accountA := newTestOAuthAccount(9201, map[string]any{codexFingerprintModeExtraKey: "cockpit"})
	accountB := newTestOAuthAccount(9202, map[string]any{codexFingerprintModeExtraKey: "cockpit"})

	conversationA1 := captureLocalCockpitIdentity(t, accountA, "thread-a", "cache-a", "session-shared", nil)
	conversationA2 := captureLocalCockpitIdentity(t, accountA, "thread-a", "cache-a", "session-shared", nil)
	retryA1 := captureLocalCockpitIdentity(t, accountA, "thread-a", "cache-a", "session-shared", conversationA1.ids)
	conversationB := captureLocalCockpitIdentity(t, accountA, "thread-b", "cache-b", "session-shared", nil)
	failoverA := captureLocalCockpitIdentity(t, accountB, "thread-a", "cache-a", "session-shared", nil)

	for _, snapshot := range []localCockpitIdentitySnapshot{conversationA1, conversationA2, retryA1, conversationB, failoverA} {
		requireLocalCockpitCarriersMatch(t, snapshot)
	}

	// New turns in one conversation keep the stable topology and cache key.
	require.Equal(t, conversationA1.ids.installationID, conversationA2.ids.installationID)
	require.Equal(t, conversationA1.ids.sessionID, conversationA2.ids.sessionID)
	require.Equal(t, conversationA1.ids.threadID, conversationA2.ids.threadID)
	require.Equal(t, conversationA1.ids.promptCacheKey, conversationA2.ids.promptCacheKey)
	require.NotEqual(t, conversationA1.ids.turnID, conversationA2.ids.turnID)

	// An internal retry reuses the exact snapshot, including its turn identity.
	require.Equal(t, conversationA1.headers, retryA1.headers)
	require.Equal(t, conversationA1.body, retryA1.body)

	// Different conversations on one account share only the installation identity.
	require.Equal(t, conversationA1.ids.installationID, conversationB.ids.installationID)
	require.NotEqual(t, conversationA1.ids.sessionID, conversationB.ids.sessionID)
	require.NotEqual(t, conversationA1.ids.threadID, conversationB.ids.threadID)
	require.NotEqual(t, conversationA1.ids.promptCacheKey, conversationB.ids.promptCacheKey)

	// Failover must regenerate all server-owned Cockpit identities for the
	// selected account, including the derived upstream cache key.
	require.True(t, conversationA1.ids.stagedAccountBound)
	require.Equal(t, accountA.ID, conversationA1.ids.stagedAccountID)
	require.True(t, failoverA.ids.stagedAccountBound)
	require.Equal(t, accountB.ID, failoverA.ids.stagedAccountID)
	require.NotEqual(t, conversationA1.ids.installationID, failoverA.ids.installationID)
	require.NotEqual(t, conversationA1.ids.sessionID, failoverA.ids.sessionID)
	require.NotEqual(t, conversationA1.ids.threadID, failoverA.ids.threadID)
	require.NotEqual(t, conversationA1.ids.promptCacheKey, failoverA.ids.promptCacheKey)
}
