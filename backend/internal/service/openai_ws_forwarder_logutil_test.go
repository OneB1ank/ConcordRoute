package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResolveOpenAIWSSessionHeadersUsesCodexHeaderBeforeExplicitSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("session_id", "explicit-session")
	c.Request.Header.Set(codexSessionIDHeader, "codex-session")

	resolution := resolveOpenAIWSSessionHeaders(c, "prompt-cache")

	require.Equal(t, "codex-session", resolution.SessionID)
	require.Equal(t, "header_session-id", resolution.SessionSource)
}

func TestResolveOpenAIWSSessionHeadersUsesCodexHeaderBeforeConversation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("conversation_id", "explicit-conversation")
	c.Request.Header.Set(codexSessionIDHeader, "codex-session")

	resolution := resolveOpenAIWSSessionHeaders(c, "prompt-cache")

	require.Equal(t, "codex-session", resolution.SessionID)
	require.Equal(t, "header_session-id", resolution.SessionSource)
	require.Equal(t, "explicit-conversation", resolution.ConversationID)
	require.Equal(t, "header_conversation_id", resolution.ConversationSource)
}

func TestResolveOpenAIWSSessionHeadersUsesCodexHeaderBeforePromptCacheKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set(codexSessionIDHeader, "codex-session")

	resolution := resolveOpenAIWSSessionHeaders(c, "prompt-cache")

	require.Equal(t, "codex-session", resolution.SessionID)
	require.Equal(t, "header_session-id", resolution.SessionSource)
}
