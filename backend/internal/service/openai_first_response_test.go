package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 首块可能只是半行创建通知；展示计时不得等待解析、正文或下游 Flush。
func TestOpenAIFirstResponseHTTPPartialChunk(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[raw], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			reader, writer := io.Pipe()
			defer func() { _ = reader.Close() }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() { _ = writer.Close() }()
				_, _ = io.WriteString(writer, "data:")
				time.Sleep(160 * time.Millisecond)
				_, _ = io.WriteString(writer, " {\"type\":\"response.created\"}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
			}()
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			account := &Account{ID: 1, Platform: PlatformOpenAI}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
			started := time.Now()
			var result OpenAIForwardResult
			result.Stream = true
			if raw {
				got, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, started, "test", "test")
				require.NoError(t, err)
				result.FirstResponseMs = got.firstResponseMs
				result.FirstTokenMs, result.SemanticFirstTokenMs = got.firstTokenMs, got.semanticFirstTokenMs
			} else {
				got, err := svc.handleStreamingResponse(context.Background(), resp, c, account, started, "test", "test")
				require.NoError(t, err)
				result.FirstResponseMs = got.firstResponseMs
				result.FirstTokenMs, result.SemanticFirstTokenMs = got.firstTokenMs, got.semanticFirstTokenMs
			}
			<-done
			require.NotNil(t, result.usageFirstTokenMs(account))
			require.GreaterOrEqual(t, *result.FirstTokenMs-*result.usageFirstTokenMs(account), 120, "首块已到，不应等待完整 SSE 行；比较间隔而非 goroutine 启动耗时")
			require.GreaterOrEqual(t, *result.FirstTokenMs, 150, "真实首内容保持独立")
		})
	}
}

// Read 可以同时返回数据和 EOF；零字节、仅响应头与本地写出不产生样本。
func TestOpenAIFirstResponseReaderBoundaries(t *testing.T) {
	started := time.Now().Add(-time.Second)
	r := &openAIFirstResponseReader{reader: strings.NewReader(""), started: started}
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	require.Zero(t, n)
	require.ErrorIs(t, err, io.EOF)
	require.Nil(t, r.snapshot())

	r.reader = firstResponseEOFReader{}
	n, err = r.Read(buf)
	require.Equal(t, 1, n)
	require.ErrorIs(t, err, io.EOF)
	first := r.snapshot()
	require.NotNil(t, first)
	require.GreaterOrEqual(t, *first, 1000, "首块晚到时必须如实保持高延迟")
	r.reader = strings.NewReader("data: later")
	_, _ = r.Read(buf)
	require.Equal(t, first, r.snapshot(), "后续读取不覆盖首次时间")
	*first = -1
	require.GreaterOrEqual(t, *r.snapshot(), 1000, "快照不共享可变指针")
	other := &openAIFirstResponseReader{reader: strings.NewReader(""), started: started}
	require.Nil(t, other.snapshot(), "每次尝试独立，不沿用失败尝试样本")
}

type firstResponseEOFReader struct{}

func (firstResponseEOFReader) Read(p []byte) (int, error) {
	p[0] = ':'
	return 1, io.EOF
}

// 异步 SSE 扫描与超时收尾同时读取样本时保持无数据竞争。
func TestOpenAIFirstResponseConcurrentSnapshot(t *testing.T) {
	r := &openAIFirstResponseReader{reader: strings.NewReader("data"), started: time.Now()}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = r.snapshot()
		}
	}()
	_, err := r.Read(make([]byte, 4))
	require.NoError(t, err)
	wg.Wait()
	require.NotNil(t, r.snapshot())
}

// 入库只切换展示值，真实首内容、计费与其它平台的口径不变。
func TestOpenAIFirstResponseUsagePriority(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI}
	result := &OpenAIForwardResult{Stream: true, FirstResponseMs: valuePtr(120),
		SemanticFirstTokenMs: valuePtr(500), FirstTokenMs: valuePtr(8000)}
	require.Equal(t, 120, *result.usageFirstTokenMs(account))
	require.Equal(t, 8000, *result.FirstTokenMs)
	result.FirstResponseMs = valuePtr(0)
	require.Zero(t, *result.usageFirstTokenMs(account), "真实 0ms 是有效观测")
	result.Stream = false
	require.Equal(t, 8000, *result.usageFirstTokenMs(account))
	result.Stream = true
	account.Platform = PlatformGrok
	require.Equal(t, 8000, *result.usageFirstTokenMs(account))
}

// 真正入库的选择也要验证：展示区间一致，但原结果仍供内部真实内容统计使用。
func TestOpenAIFirstResponseRecordUsage(t *testing.T) {
	repo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(repo,
		&openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}},
		&openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	result := &OpenAIForwardResult{
		RequestID: "resp_chunk_usage", Model: "gpt-5.1", Stream: true, OpenAIWSMode: true,
		Duration: 4 * time.Second, ResponseDuration: 9 * time.Second,
		FirstResponseMs: valuePtr(5000), SemanticFirstTokenMs: valuePtr(500), FirstTokenMs: valuePtr(3000),
	}
	require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: result, APIKey: &APIKey{ID: 1000, Group: &Group{RateMultiplier: 1}},
		User: &User{ID: 2000}, Account: &Account{ID: 3000, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		APIKeyService: &openAIRecordUsageAPIKeyQuotaStub{},
	}))
	require.Equal(t, 5000, *repo.lastLog.FirstTokenMs)
	require.Equal(t, 9000, *repo.lastLog.DurationMs)
	require.Equal(t, 3000, *result.FirstTokenMs)
	require.Equal(t, 4*time.Second, result.Duration)
	require.Zero(t, repo.lastLog.TotalCost)
}
