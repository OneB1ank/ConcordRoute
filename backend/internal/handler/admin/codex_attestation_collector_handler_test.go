package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexAttestationCollectorHandlerLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	collector := service.NewCodexAppServerAttestationCollector()
	handler := NewCodexAttestationCollectorHandler(nil, collector)

	startRecorder := httptest.NewRecorder()
	startCtx, _ := gin.CreateTestContext(startRecorder)
	handler.Start(startCtx)
	require.Equal(t, 200, startRecorder.Code)

	sessionRecorder := httptest.NewRecorder()
	sessionCtx, _ := gin.CreateTestContext(sessionRecorder)
	handler.CreateSession(sessionCtx)
	require.Equal(t, 200, sessionRecorder.Code)
}
