package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/TokenFlux/TokenRouter/internal/pkg/logger"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type ttftWithCountingCore struct {
	zapcore.Core
	calls *atomic.Int64
}

func (c ttftWithCountingCore) With(fields []zapcore.Field) zapcore.Core {
	c.calls.Add(1)
	return ttftWithCountingCore{Core: c.Core.With(fields), calls: c.calls}
}

// Info 关闭时不应构造/序列化诊断字段；Gin 错误的 Warn 输出仍然保留。
func TestLoggerTTFTDisabledLevelSkipsFields(t *testing.T) {
	for _, withError := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_error", true: "warn_error"}[withError], func(t *testing.T) {
			var output bytes.Buffer
			var calls atomic.Int64
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(&output), zap.WarnLevel)
			l := zap.New(ttftWithCountingCore{Core: core, calls: &calls})
			router := gin.New()
			router.Use(Logger())
			router.GET("/measure", func(c *gin.Context) {
				service.InitTTFTStageTiming(c, true)
				mark := service.BeginTTFTUpstreamAttempt(c, 1)
				mark("first_content_received")
				if withError {
					_ = c.Error(errors.New("synthetic error"))
				}
				c.Status(200)
			})
			req := httptest.NewRequest("GET", "/measure", nil)
			req = req.WithContext(logger.IntoContext(req.Context(), l))
			router.ServeHTTP(httptest.NewRecorder(), req)
			if withError {
				require.Contains(t, output.String(), "http request contains gin errors")
				require.Contains(t, output.String(), "ttft_attempts")
			} else {
				require.Empty(t, output.String())
				require.Zero(t, calls.Load(), "disabled log must not eagerly serialize diagnostic fields")
			}
		})
	}
}

type ttftBlockingLogWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *ttftBlockingLogWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(data), nil
}

// 即使日志输出端阻塞，诊断完成日志也只能在处理器已 Flush 首内容后写出。
// 这不保证请求结束或后续请求不受慢磁盘影响，因此不把同步日志描述成零成本。
func TestLoggerTTFTBlockedSinkAfterFirstFlush(t *testing.T) {
	writer := &ttftBlockingLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
	l := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(writer), zap.InfoLevel))
	router := gin.New()
	router.Use(Logger())
	router.GET("/measure", func(c *gin.Context) {
		service.InitTTFTStageTiming(c, true)
		latencytrace.Start(c.Request.Context(), "account_slot")(nil)
		mark := service.BeginTTFTUpstreamAttempt(c, 1)
		mark("first_content_received")
		_, _ = c.Writer.WriteString("first content")
		c.Writer.Flush()
		mark("first_content_flush_completed")
	})
	req := httptest.NewRequest("GET", "/measure", nil)
	req = req.WithContext(logger.IntoContext(req.Context(), l))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(response, req)
	}()
	t.Cleanup(func() {
		close(writer.release)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("handler did not finish after releasing log output")
		}
	})
	select {
	case <-writer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("completion log was not reached")
	}
	require.True(t, response.Flushed)
	require.Equal(t, "first content", response.Body.String())
	select {
	case <-done:
		t.Fatal("expected completion log to remain blocked")
	default:
	}
}

// 请求结束时记录一次 JSON 日志，输出到 Discard；分开测日志关闭与诊断关闭。
func BenchmarkLoggerTTFTOverhead(b *testing.B) {
	for _, level := range []zapcore.Level{zap.InfoLevel, zap.WarnLevel} {
		for _, enabled := range []bool{false, true} {
			b.Run(level.String()+"/"+map[bool]string{false: "off", true: "on"}[enabled], func(b *testing.B) {
				l := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), level))
				router := gin.New()
				router.Use(Logger())
				router.GET("/measure", func(c *gin.Context) {
					service.InitTTFTStageTiming(c, enabled)
					latencytrace.Start(c.Request.Context(), "account_slot")(nil)
					mark := service.BeginTTFTUpstreamAttempt(c, 1)
					mark("first_content_received")
					mark("first_content_flush_completed")
					mark("stream_completed")
					c.Status(200)
				})
				req := httptest.NewRequest("GET", "/measure", nil)
				req = req.WithContext(logger.IntoContext(req.Context(), l))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					router.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		}
	}
}
