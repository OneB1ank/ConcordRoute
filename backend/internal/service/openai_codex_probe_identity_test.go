//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveCodexProbeFingerprintIDsStableConversationRotatingTurn(t *testing.T) {
	account := newTestOAuthAccount(701, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintCockpit),
	})

	first := resolveCodexProbeFingerprintIDs(account, codexProbePurposeAccountTest, "GPT-5.4")
	second := resolveCodexProbeFingerprintIDs(account, codexProbePurposeAccountTest, "gpt-5.4")
	other := resolveCodexProbeFingerprintIDs(account, codexProbePurposeNativeCompactionV2, "gpt-5.4")

	require.NotNil(t, first)
	require.NotNil(t, second)
	require.NotNil(t, other)
	require.Equal(t, first.installationID, second.installationID)
	require.Equal(t, first.sessionID, second.sessionID)
	require.Equal(t, first.threadID, second.threadID)
	require.Equal(t, first.windowID, second.windowID)
	require.Equal(t, first.promptCacheKey, second.promptCacheKey)
	require.NotEqual(t, first.turnID, second.turnID)
	require.NotEqual(t, first.threadID, other.threadID)
	require.NotEqual(t, first.promptCacheKey, other.promptCacheKey)
}

func TestApplyCodexProbeFingerprintUsesSameIDsInHeaderAndBody(t *testing.T) {
	account := newTestOAuthAccount(702, map[string]any{
		codexFingerprintModeExtraKey: string(codexFingerprintCockpit),
	})
	ids := resolveCodexProbeFingerprintIDs(account, codexProbePurposeAccountTest, "gpt-5.4")
	payload := map[string]any{"model": "gpt-5.4"}
	headers := make(http.Header)

	require.True(t, applyCodexFingerprintClientMetadata(payload, ids))
	applyCodexProbeFingerprintHeaders(headers, ids)

	metadata, ok := payload["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, ids.installationID, headers.Get("x-codex-installation-id"))
	require.Equal(t, ids.sessionID, headers.Get("session-id"))
	require.Equal(t, ids.threadID, headers.Get("thread-id"))
	require.Equal(t, ids.windowID, headers.Get("x-codex-window-id"))
	require.Equal(t, ids.promptCacheKey, headers.Get("conversation_id"))
	require.Equal(t, ids.installationID, metadata["x-codex-installation-id"])
	require.Equal(t, ids.sessionID, metadata["session_id"])
	require.Equal(t, ids.threadID, metadata["thread_id"])
	require.Equal(t, ids.turnID, metadata["turn_id"])
	require.Equal(t, ids.windowID, metadata["x-codex-window-id"])
	require.Equal(t, ids.promptCacheKey, payload["prompt_cache_key"])

	var turnMetadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(headers.Get("x-codex-turn-metadata")), &turnMetadata))
	require.Equal(t, ids.installationID, turnMetadata["installation_id"])
	require.Equal(t, ids.sessionID, turnMetadata["session_id"])
	require.Equal(t, ids.threadID, turnMetadata["thread_id"])
	require.Equal(t, ids.turnID, turnMetadata["turn_id"])
	require.Equal(t, ids.windowID, turnMetadata["window_id"])
	require.Equal(t, ids.promptCacheKey, turnMetadata["prompt_cache_key"])
}

func TestResolveCodexProbeFingerprintIDsRespectsModes(t *testing.T) {
	tests := []struct {
		mode            codexFingerprintMode
		wantIDs         bool
		wantSession     bool
		wantPromptCache bool
	}{
		{mode: codexFingerprintOff, wantIDs: false},
		{mode: codexFingerprintDevice, wantIDs: true},
		{mode: codexFingerprintSession, wantIDs: true, wantSession: true},
		{mode: codexFingerprintCockpit, wantIDs: true, wantSession: true, wantPromptCache: true},
		{mode: codexFingerprintFull, wantIDs: true, wantSession: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			account := newTestOAuthAccount(703, map[string]any{codexFingerprintModeExtraKey: string(tt.mode)})
			ids := resolveCodexProbeFingerprintIDs(account, codexProbePurposeAccountTest, "gpt-5.4")
			if !tt.wantIDs {
				require.Nil(t, ids)
				return
			}
			require.NotNil(t, ids)
			require.NotEmpty(t, ids.installationID)
			require.Equal(t, tt.wantSession, ids.sessionID != "")
			require.Equal(t, tt.wantPromptCache, ids.promptCacheKey != "")
		})
	}
}
