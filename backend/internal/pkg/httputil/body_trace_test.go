package httputil

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/stretchr/testify/require"
)

// 读取阶段必须包含网络等待，输出只含阶段、时间和失败标记。
func TestReadRequestBodyWithPrealloc_TracePreservesBodyAndErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		rec := latencytrace.New(time.Now())
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("PRIVATE_BODY"))
		req = req.WithContext(latencytrace.WithRecorder(context.Background(), rec))
		if fail {
			req.Body = io.NopCloser(traceFailedReader{})
		}
		body, err := ReadRequestBodyWithPrealloc(req)
		if fail {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.Equal(t, "PRIVATE_BODY", string(body))
		}
		events := rec.Snapshot().Events
		require.Len(t, events, 2)
		require.Equal(t, "request_body_read_started", events[0].Phase)
		require.Equal(t, "request_body_read_done", events[1].Phase)
		require.Equal(t, fail, events[1].Failed)
		raw, err := json.Marshal(events)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "PRIVATE")
	}
}

type traceFailedReader struct{}

func (traceFailedReader) Read([]byte) (int, error) {
	return 0, errors.New("PRIVATE_READ_ERROR")
}

// 同一读取实现分别测量诊断关闭/开启的开销，不发送网络或模型请求。
func BenchmarkRequestBodyDiagnostics(b *testing.B) {
	body := strings.Repeat("x", 1<<20)
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
				if enabled {
					req = req.WithContext(latencytrace.WithRecorder(req.Context(), latencytrace.New(time.Now())))
				}
				got, err := ReadRequestBodyWithPrealloc(req)
				if err != nil || len(got) != len(body) {
					b.Fatal("请求体读取异常")
				}
			}
		})
	}
}
