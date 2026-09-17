//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
	"github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// audioHandlerUpstream 仅替换网络边界，保留真实 handler、调度、计量和用量记录链。
type audioHandlerUpstream struct {
	service.HTTPUpstream
	status int
	calls  int
	path   string
}

func (u *audioHandlerUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	u.path = req.URL.Path
	return &http.Response{
		StatusCode: u.status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"text":"合成转录结果"}`)),
	}, nil
}

type audioHandlerUsageRepo struct {
	service.UsageLogRepository
	logs []*service.UsageLog
}

func (r *audioHandlerUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.logs = append(r.logs, log)
	return true, nil
}

// audioHandlerUpload 使用一秒静音 WAV，不录制或上传真实用户语音。
func audioHandlerUpload(t *testing.T, desktop bool) ([]byte, string) {
	t.Helper()
	wav := make([]byte, 32044)
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(len(wav)-8))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], 32000)
	var b bytes.Buffer
	writer := multipart.NewWriter(&b)
	if !desktop {
		require.NoError(t, writer.WriteField("model", "gpt-transcribe"))
	}
	file, err := writer.CreateFormFile("file", "silence.wav")
	require.NoError(t, err)
	_, err = file.Write(wav)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return b.Bytes(), writer.FormDataContentType()
}

func TestAudioTranscriptionHandlerUploadAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{service.AccountTypeOAuth, service.AccountTypeAPIKey} {
		for _, status := range []int{200, 403} {
			name := kind + "/" + http.StatusText(status)
			t.Run(name, func(t *testing.T) {
				cfg := &config.Config{RunMode: config.RunModeSimple}
				cfg.Default.RateMultiplier = 1
				account := service.Account{
					ID: 51, Platform: service.PlatformOpenAI, Type: kind,
					Status: service.StatusActive, Schedulable: true, Concurrency: 1,
					Credentials: map[string]any{
						"access_token": "synthetic-oauth", "chatgpt_account_id": "synthetic-account",
						"api_key": "synthetic-api-key",
					},
				}
				// 复用已有内存账号仓库；不连接数据库或真实账号。
				repo := &grokCredentialHandlerRepo{accounts: []service.Account{account}}
				upstream := &audioHandlerUpstream{status: status}
				usage := &audioHandlerUsageRepo{}
				billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				defer billing.Stop()
				gateway := service.NewOpenAIGatewayService(
					repo, usage, nil, nil, nil, nil, nil, cfg, nil, nil,
					service.NewBillingService(cfg, nil), nil, billing, upstream,
					nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil,
				)
				cache := &concurrencyCacheMock{
					acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
					acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
				}
				h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(cache), billing,
					&service.APIKeyService{}, nil, nil, nil, nil, cfg)
				groupID, price := int64(1), 3.6
				key := &service.APIKey{
					ID: 52, GroupID: &groupID, User: &service.User{ID: 53, Status: service.StatusActive},
					Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI,
						AllowAudioTranscription: true, RateMultiplier: 1, AudioSTTPricePerHour: &price},
				}
				r := gin.New()
				r.Use(func(c *gin.Context) {
					c.Set(string(middleware.ContextKeyAPIKey), key)
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: 1})
				})
				desktop := kind == service.AccountTypeOAuth
				path, upstreamPath := "/v1/audio/transcriptions", "/v1/audio/transcriptions"
				if desktop {
					path, upstreamPath = "/transcribe", "/backend-api/transcribe"
				}
				r.POST(path, h.AudioTranscriptions)
				body, ct := audioHandlerUpload(t, desktop)
				req := httptest.NewRequest("POST", path, bytes.NewReader(body))
				req.Header.Set("Content-Type", ct)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				require.Equal(t, 1, upstream.calls, "音频不跨账号自动重传")
				require.Equal(t, upstreamPath, upstream.path)
				if status != 200 {
					require.Equal(t, 502, w.Code, w.Body.String())
					require.Empty(t, usage.logs, "上游拒绝不记成功用量")
					require.NotContains(t, w.Body.String(), "合成转录结果")
					return
				}
				require.Equal(t, 200, w.Code, w.Body.String())
				require.JSONEq(t, `{"text":"合成转录结果"}`, w.Body.String())
				require.Len(t, usage.logs, 1, "处理器确实提交计量结果，不只测试转发函数")
				require.InDelta(t, 0.001, usage.logs[0].ActualCost, 1e-10)
				require.True(t, strings.HasPrefix(usage.logs[0].RequestID, "openai_audio:"))
				require.Zero(t, usage.logs[0].InputTokens)
				require.Zero(t, usage.logs[0].OutputTokens)
			})
		}
	}
}
