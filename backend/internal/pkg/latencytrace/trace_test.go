package latencytrace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 禁用无状态；上下文取消和已有 trace 保留，采样不泄漏地址及错误原文。
func TestTraceDisabledCompositionAndPrivacy(t *testing.T) {
	ctx := context.Background()
	require.True(t, ctx == WithHTTPTrace(ctx, nil))
	require.Zero(t, testing.AllocsPerRun(100, func() { Start(ctx, "token_get")(nil) }))
	calls := 0
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotFirstResponseByte: func() { calls++ }})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	recorder := New(time.Now())
	ctx = WithHTTPTrace(WithRecorder(ctx, recorder), func() { calls++ })
	trace := httptrace.ContextClientTrace(ctx)
	trace.DNSStart(httptrace.DNSStartInfo{Host: "PRIVATE_HOST"})
	trace.DNSDone(httptrace.DNSDoneInfo{Err: errors.New("PRIVATE_ERROR")})
	trace.GotFirstResponseByte()
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("PRIVATE_TOKEN")})
	Mark(ctx, "PRIVATE_PHASE", nil)
	require.Equal(t, 2, calls)
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	data, err := json.Marshal(recorder.Snapshot())
	require.NoError(t, err)
	require.NotContains(t, string(data), "PRIVATE")
	require.Contains(t, string(data), `"failed":true`)
}

// 单连接被占用时，等待落在 get→got，而不是误计成请求上传后等待。
func TestTraceRealConnectionPoolWait(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })
	transport := &http.Transport{MaxConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	firstDone := make(chan error, 1)
	go func() {
		resp, err := client.Get(server.URL + "/hold")
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		firstDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not enter")
	}
	recorder := New(time.Now())
	gotGet := make(chan struct{}, 1)
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GetConn: func(string) {
		select {
		case gotGet <- struct{}{}:
		default:
		}
	}})
	ctx = WithHTTPTrace(WithRecorder(ctx, recorder), nil)
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	require.NoError(t, err)
	secondDone := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		secondDone <- err
	}()
	select {
	case <-gotGet:
	case <-time.After(2 * time.Second):
		once.Do(func() { close(release) })
		t.Fatal("second request did not ask for connection")
	}
	time.Sleep(60 * time.Millisecond)
	once.Do(func() { close(release) })
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	phases := map[string]Event{}
	for _, event := range recorder.Snapshot().Events {
		phases[event.Phase] = event
	}
	require.GreaterOrEqual(t, phases["connection_got"].AtMS-phases["connection_get"].AtMS, int64(50))
	require.True(t, *phases["connection_got"].Reused)
	require.NotContains(t, phases, "tcp_connect_started")
	data, err := json.Marshal(recorder.Snapshot())
	require.NoError(t, err)
	t.Log(string(data))
}

type delayedUploadReader struct {
	io.Reader
	once sync.Once
}

func (r *delayedUploadReader) Read(p []byte) (int, error) {
	r.once.Do(func() { time.Sleep(60 * time.Millisecond) })
	return r.Reader.Read(p)
}

// 原生 TLS 成功链路把请求体上传等待与服务端响应头等待分别记录。
func TestTraceNativeTLSAndUploadDelay(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "hello", string(body))
		time.Sleep(40 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	recorder := New(time.Now())
	ctx := WithHTTPTrace(WithRecorder(context.Background(), recorder), nil)
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL, &delayedUploadReader{Reader: strings.NewReader("hello")})
	require.NoError(t, err)
	req.ContentLength = 5
	client := server.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	phases := map[string]Event{}
	for _, event := range recorder.Snapshot().Events {
		phases[event.Phase] = event
	}
	require.Contains(t, phases, "tls_done")
	require.False(t, phases["tls_done"].Failed)
	require.GreaterOrEqual(t, phases["request_written"].AtMS-phases["request_headers_written"].AtMS, int64(50))
	require.GreaterOrEqual(t, phases["first_response_byte"].AtMS-phases["request_written"].AtMS, int64(30))
	data, err := json.Marshal(recorder.Snapshot())
	require.NoError(t, err)
	t.Log(string(data))
}

// 多路连接回调保留重复事件，超量显式计数，快照不共享可变指针。
func TestTraceBoundedConcurrentSnapshots(t *testing.T) {
	recorder := New(time.Now())
	ctx := WithHTTPTrace(WithRecorder(context.Background(), recorder), nil)
	trace := httptrace.ContextClientTrace(ctx)
	trace.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: time.Second})
	first := recorder.Snapshot()
	*first.Events[0].Reused = false
	require.True(t, *recorder.Snapshot().Events[0].Reused)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			trace.ConnectStart("tcp", "PRIVATE")
			trace.ConnectDone("tcp", "PRIVATE", nil)
			_ = recorder.Snapshot()
		}()
	}
	wg.Wait()
	snapshot := recorder.Snapshot()
	require.Len(t, snapshot.Events, maxEvents)
	require.Equal(t, 401-maxEvents, snapshot.Dropped)
	for i := 1; i < len(snapshot.Events); i++ {
		require.GreaterOrEqual(t, snapshot.Events[i].AtMS, snapshot.Events[i-1].AtMS)
	}
}

// 测量的是实际等待区间，不把预算值写成实际耗时；重复结束不会重复累计。
func TestTraceOperationDurationAndCancellation(t *testing.T) {
	recorder := New(time.Now())
	ctx := WithRecorder(context.Background(), recorder)
	done := Start(ctx, "account_slot")
	time.Sleep(40 * time.Millisecond)
	done(context.DeadlineExceeded)
	done(nil)
	events := recorder.Snapshot().Events
	require.Len(t, events, 2)
	require.True(t, events[1].Failed)
	require.GreaterOrEqual(t, events[1].AtMS-events[0].AtMS, int64(35))
}

func TestTracePersistenceAndIngressDisabledHaveNoSamplingAllocations(t *testing.T) {
	ctx := context.Background()
	allocations := testing.AllocsPerRun(100, func() {
		for _, phase := range []string{
			"identity_lock_wait", "identity_binding_read", "identity_binding_merge",
			"identity_binding_write", "request_body_read", "api_key_auth",
		} {
			Start(ctx, phase)(nil)
		}
	})
	require.Zero(t, allocations, "关闭诊断时新增阶段不创建采样对象")
}
