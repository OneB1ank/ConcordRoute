package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestTTFTDiagnosticsRecordsGatewayStagesAndCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TTFTDiagnostics(true))
	var captured *service.TTFTStageTiming
	r.POST("/v1/responses", func(c *gin.Context) {
		value, _ := c.Get(service.OpsTTFTStageTimingKey)
		captured, _ = value.(*service.TTFTStageTiming)
		service.MarkTTFTStage(c, "forward_started")
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, captured)
	require.Contains(t, captured.Snapshot(time.Now()), "request_received")
	require.Contains(t, captured.Snapshot(time.Now()), "request_completed")

	// The middleware owns the context; exercise the same lifecycle separately
	// to verify its completion marker is emitted only for gateway paths.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.InitTTFTStageTiming(c, true)
	service.MarkTTFTStage(c, "forward_started")
	service.MarkTTFTStage(c, "stream_completed")
	snapshot := service.TTFTStageTimingSnapshot(c, time.Now())
	require.Contains(t, snapshot, "request_received")
	require.Contains(t, snapshot, "stream_completed")
}

func TestTTFTDiagnosticsSkipsNonGatewayPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TTFTDiagnostics(true))
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestTTFTDiagnosticsIncludesMessagesPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(TTFTDiagnostics(true))
	var captured *service.TTFTStageTiming
	r.POST("/v1/messages", func(c *gin.Context) {
		value, _ := c.Get(service.OpsTTFTStageTimingKey)
		captured, _ = value.(*service.TTFTStageTiming)
		service.MarkTTFTStage(c, "request_body_read")
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, captured)
	require.Contains(t, captured.Snapshot(time.Now()), "request_body_read")
}
