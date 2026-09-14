package repository

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/latencytrace"
	"github.com/stretchr/testify/require"
)

// 真实本地 HTTP 链路复现观测缺口，并区分响应头延迟与热连接复用。
func TestHTTPUpstreamLatencyTraceColdWarmAndHeaderWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "application/json", r.Header.Get("Content-Type"))
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	upstream := NewHTTPUpstream(nil)
	for i := 0; i < 2; i++ {
		recorder := latencytrace.New(time.Now())
		ctx := latencytrace.WithHTTPTrace(latencytrace.WithRecorder(context.Background(), recorder), nil)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := upstream.Do(req, "", 51, 1)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, "ok", string(body))
		phases := map[string]latencytrace.Event{}
		for _, event := range recorder.Snapshot().Events {
			phases[event.Phase] = event
		}
		t.Logf("round=%d trace=%+v", i, recorder.Snapshot())
		require.Contains(t, phases, "host_validation_done")
		require.Contains(t, phases, "client_acquire_done")
		require.Contains(t, phases, "connection_got")
		require.Equal(t, i == 1, *phases["connection_got"].Reused)
		require.GreaterOrEqual(t, phases["first_response_byte"].AtMS-phases["request_written"].AtMS, int64(50))
		if i == 1 {
			require.NotContains(t, phases, "tcp_connect_started")
		}
	}
}
