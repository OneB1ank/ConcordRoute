package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const audioTranscriptionTimeout = 110 * time.Second

// ForwardAudioTranscription 复用账号出口、认证与 TLS 路由；不创建另一套浏览器身份或重放证明。
func (s *OpenAIGatewayService) ForwardAudioTranscription(
	ctx context.Context, c *gin.Context, account *Account, p *OpenAIAudioTranscriptionRequest,
	mappedModel string, match TLSFingerprintRouterMatchResult,
) (*OpenAIForwardResult, error) {
	if s == nil || c == nil || p == nil || p.DurationSeconds <= 0 || p.DurationSeconds > 600 ||
		math.IsNaN(p.DurationSeconds) || math.IsInf(p.DurationSeconds, 0) {
		return nil, audioRequestError(400, "valid measured audio duration is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityAudioTranscriptions) {
		return nil, audioRequestError(400, "account does not support audio transcription")
	}
	ctx, cancel := context.WithTimeout(ctx, audioTranscriptionTimeout)
	defer cancel()
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, audioRequestError(502, "transcription credential unavailable")
	}
	if strings.TrimSpace(token) == "" {
		return nil, audioRequestError(502, "transcription credential unavailable")
	}
	model := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, p.Model, mappedModel))
	oauth := account.Type == AccountTypeOAuth
	endpoint := "/v1/audio/transcriptions"
	target := "https://chatgpt.com/backend-api/transcribe"
	if oauth {
		endpoint = "/backend-api/transcribe"
		// OAuth 端点由上游决定识别模型，不声称客户端别名就是实际识别模型。
		model = ""
	} else {
		base := account.GetOpenAIBaseURL()
		if base == "" {
			base = "https://api.openai.com"
		}
		validated, err := s.validateUpstreamBaseURL(base)
		if err != nil {
			return nil, audioRequestError(502, "invalid transcription upstream URL")
		}
		target = buildOpenAIEndpointURL(validated, endpoint)
	}
	body, contentType, err := p.upstreamBody(oauth, model)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI))
	headers, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, audioRequestError(502, "transcription credential unavailable")
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	if oauth {
		if err := resolveAndSetOpenAIChatGPTAccountHeaders(ctx, s.accountRepo, req.Header, account); err != nil {
			return nil, audioRequestError(502, "transcription account context unavailable")
		}
		req.Header.Set("Originator", resolveCodexOutboundIdentity("").originator)
		s.applyOpenAIUpstreamUserAgent(ctx, c, account, req, true, match)
		enforceCodexIdentityHeadersWithUA(req.Header, s.codexIdentityOverrideUA(account, match))
	} else if ua := account.GetOpenAIUserAgent(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	account.ApplyHeaderOverrides(req.Header)
	// 转录没有 Responses 会话、turn 或缓存语义；只保留本接口需要的标准头。
	stripOpenAIAlphaSearchResponsesHeaders(req.Header)
	req.Header.Del("X-Oai-Attestation")
	req.Header.Del("X-Codex-Turn-Metadata")
	req.Header.Set("Content-Type", contentType)
	if oauth {
		s.rememberOpenAIOutboundIdentity(account, req.Header.Get("User-Agent"), match)
	}
	SetActualOpenAIUpstreamEndpoint(c, endpoint)
	started := time.Now()
	resp, err := s.httpUpstream.DoWithTLS(req, resolveAccountProxyURL(account), account.ID,
		account.Concurrency, s.resolveOpenAITLSProfile(account, match))
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(started).Milliseconds())
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		setOpsUpstreamError(c, 502, "transcription transport failed", "")
		return nil, audioRequestError(502, "transcription upstream connection failed")
	}
	// 关闭错误不覆盖已取得的转录响应；请求读取失败仍由下方路径处理。
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, audioRequestError(502, "invalid transcription response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 不向多个账号自动重复上传音频，也不因听写权限不足封禁可用的推理账号。
		setOpsUpstreamError(c, resp.StatusCode, fmt.Sprintf("transcription upstream HTTP %d", resp.StatusCode), "")
		c.Header("X-Transcription-Upstream-Status", fmt.Sprint(resp.StatusCode))
		status := http.StatusBadGateway
		if resp.StatusCode == 429 {
			status = 429
		}
		if resp.StatusCode == 413 {
			status = 413
		}
		return nil, audioRequestError(status, "transcription upstream rejected the request")
	}
	responseType := "application/json"
	if !oauth && p.ResponseFormat == "text" {
		media := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
		if media != "text/plain" {
			return nil, audioRequestError(502, "invalid transcription text response")
		}
		responseType = "text/plain; charset=utf-8"
	} else {
		text, err := audioTranscriptionValidText(data)
		if err != nil {
			return nil, audioRequestError(502, err.Error())
		}
		if p.ResponseFormat == "text" {
			data, responseType = []byte(text), "text/plain; charset=utf-8"
		}
	}
	// 成功后才更新被动额度快照，不引入主动查额度/探活。
	if !account.IsShadow() {
		s.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, resp.Header)
	}
	c.Data(http.StatusOK, responseType, data)
	return &OpenAIForwardResult{
		RequestID: "openai_audio:" + generateRequestID(), Model: p.Model,
		BillingModel:  resolveOpenAIForwardModel(account, p.Model, mappedModel),
		UpstreamModel: model, UpstreamEndpoint: endpoint, Duration: time.Since(started),
		AudioUsage: &AudioUsage{Mode: "stt", DurationOrUnits: p.DurationSeconds / 3600},
	}, nil
}

// AudioTranscriptionModelRestricted 沿用文本路由的计费模型来源与最终上游模型限制，避免空模型选账号扩大权限。
func (s *OpenAIGatewayService) AudioTranscriptionModelRestricted(ctx context.Context, groupID *int64, account *Account, model string) bool {
	if s.checkChannelPricingRestriction(ctx, groupID, model) {
		return true
	}
	return account != nil && groupID != nil && s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
		s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, model, false)
}

// AudioTranscriptionErrorStatus 仅输出固定的客户端说明，不回显上游正文或录音。
func AudioTranscriptionErrorStatus(err error) (int, string) {
	var requestError *AudioTranscriptionRequestError
	if errors.As(err, &requestError) {
		return requestError.Status, requestError.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 504, "transcription timed out"
	}
	return 502, "transcription request failed"
}
