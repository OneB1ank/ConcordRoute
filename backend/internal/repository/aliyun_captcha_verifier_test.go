package repository

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/service"
)

// newAliyunCaptchaTestTarget 起一个假的阿里云端点，让真实 SDK 走完整的签名/序列化链路。
func newAliyunCaptchaTestTarget(t *testing.T, handler http.HandlerFunc) (*aliyunCaptchaVerifier, service.AliyunCaptchaCredentials) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	verifier := &aliyunCaptchaVerifier{protocol: "HTTP", timeoutMillis: 2_000}
	cred := service.AliyunCaptchaCredentials{
		AccessKeyID:     "test-ak-id",
		AccessKeySecret: "test-ak-secret",
		SceneID:         "scene-1",
		Endpoint:        strings.TrimPrefix(server.URL, "http://"),
	}
	return verifier, cred
}

func TestAliyunCaptchaVerifier_VerifySuccess(t *testing.T) {
	var capturedParam, capturedSceneID string
	verifier, cred := newAliyunCaptchaTestTarget(t, func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		capturedParam = r.Form.Get("CaptchaVerifyParam")
		capturedSceneID = r.Form.Get("SceneId")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"Success","Message":"success","RequestId":"req-1","Success":true,"Result":{"VerifyResult":true,"VerifyCode":"T001"}}`))
	})

	result, err := verifier.VerifyCaptcha(context.Background(), cred, "the-verify-param")
	require.NoError(t, err)
	require.True(t, result.VerifyResult)
	require.Equal(t, "T001", result.VerifyCode)
	require.Equal(t, "the-verify-param", capturedParam)
	require.Equal(t, "scene-1", capturedSceneID)
}

func TestAliyunCaptchaVerifier_VerifyResultFalse(t *testing.T) {
	verifier, cred := newAliyunCaptchaTestTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Code":"Success","RequestId":"req-2","Success":true,"Result":{"VerifyResult":false,"VerifyCode":"F002"}}`))
	})

	result, err := verifier.VerifyCaptcha(context.Background(), cred, "bad-param")
	require.NoError(t, err)
	require.False(t, result.VerifyResult)
	require.Equal(t, "F002", result.VerifyCode)
}

func TestAliyunCaptchaVerifier_APIErrorNormalized(t *testing.T) {
	verifier, cred := newAliyunCaptchaTestTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"Code":"SignatureDoesNotMatch","Message":"Specified signature is not matched with our calculation.","RequestId":"req-3"}`))
	})

	_, err := verifier.VerifyCaptcha(context.Background(), cred, "param")
	require.Error(t, err)
	var apiErr *service.AliyunCaptchaAPIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, "SignatureDoesNotMatch", apiErr.Code)
}

func TestAliyunCaptchaVerifier_TransportError(t *testing.T) {
	// 保留 .1 上的端口所有权，访问未监听的 .2；提前关闭服务可能让其它测试
	// 复用端口并返回 HTTP 错误，而主动断开连接的 EOF 又会被 SDK 包装为 API 错误。
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	endpoint := strings.Replace(strings.TrimPrefix(server.URL, "http://"), "127.0.0.1:", "127.0.0.2:", 1)

	verifier := &aliyunCaptchaVerifier{protocol: "HTTP", timeoutMillis: 2_000}
	cred := service.AliyunCaptchaCredentials{
		AccessKeyID:     "test-ak-id",
		AccessKeySecret: "test-ak-secret",
		SceneID:         "scene-1",
		Endpoint:        endpoint,
	}

	_, err := verifier.VerifyCaptcha(context.Background(), cred, "param")
	require.Error(t, err)
	var apiErr *service.AliyunCaptchaAPIError
	require.False(t, errors.As(err, &apiErr), "transport errors must not be normalized to API errors")
}
