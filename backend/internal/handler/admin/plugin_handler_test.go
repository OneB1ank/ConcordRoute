package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type pluginConfigTestRepository struct {
	service.PluginRepository
	raw string
}

func (r *pluginConfigTestRepository) GetByID(context.Context, int64) (*service.PluginInstallation, error) {
	return &service.PluginInstallation{ID: 1, ConfigEncrypted: r.raw}, nil
}

// 仅测试接口封装；真实凭据加解密由 SecretEncryptor 的独立测试覆盖。
type pluginConfigTestEncryptor struct{}

func (pluginConfigTestEncryptor) Encrypt(s string) (string, error) { return s, nil }
func (pluginConfigTestEncryptor) Decrypt(s string) (string, error) { return s, nil }

// 插件配置允许使用 code/data/message 等字段，不应与管理员接口信封冲突。
func TestPluginGetConfigUsesResponseEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, raw := range []string{
		`{}`,
		`{"code":0,"data":{"nested":true},"message":"plugin"}`,
		`{"code":401,"message":"plugin configuration, not an API error"}`,
		`{"code":"custom","large_id":9007199254740993}`,
	} {
		t.Run(raw, func(t *testing.T) {
			manager := service.NewPluginManager(&pluginConfigTestRepository{raw: raw}, pluginConfigTestEncryptor{}, nil, service.PluginHostInfo{})
			handler := NewPluginHandler(manager)
			router := gin.New()
			router.GET("/plugins/:id/config", handler.GetConfig)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/plugins/1/config", nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			var envelope struct {
				Code    int             `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
			require.Zero(t, envelope.Code)
			require.Equal(t, "success", envelope.Message)
			require.JSONEq(t, raw, string(envelope.Data))
		})
	}
}
