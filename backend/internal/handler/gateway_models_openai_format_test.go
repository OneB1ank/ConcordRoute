package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	middleware2 "github.com/TokenFlux/TokenRouter/internal/server/middleware"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 验证模型来源和筛选状态只影响模型集合，不改变 OpenAI 模型项的协议格式。
func TestGatewayModels_OpenAIFormatAcrossCatalogSources(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mapped := service.Account{
		ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-5.5": "gpt-5.5", "custom-model": "gpt-5.5",
		}},
	}
	unmapped := service.Account{ID: 2, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	for _, tc := range []struct {
		name       string
		accounts   []service.Account
		customList service.GroupModelsListConfig
		want       []string
		exact      bool
	}{
		{name: "显式映射含自定义别名", accounts: []service.Account{mapped}, want: []string{"custom-model", "gpt-5.5"}, exact: true},
		{name: "混合账号保留默认模型", accounts: []service.Account{mapped, unmapped}, want: []string{"custom-model", "gpt-5.5", "gpt-6-astra"}},
		{name: "无映射默认模型", accounts: []service.Account{unmapped}, want: []string{"gpt-5.5", "gpt-6-astra"}},
		{
			name: "自定义列表过滤并保持顺序", accounts: []service.Account{mapped},
			customList: service.GroupModelsListConfig{Enabled: true, Models: []string{"gpt-5.5", "missing-model", "custom-model"}},
			want:       []string{"gpt-5.5", "custom-model"}, exact: true,
		},
		{name: "空账号池保持空数组", want: []string{}, exact: true},
		{
			name: "过滤后为空不补默认模型", accounts: []service.Account{mapped},
			customList: service.GroupModelsListConfig{Enabled: true, Models: []string{"missing-model"}},
			want:       []string{}, exact: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(6680)
			h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
				byGroup: map[int64][]service.Account{groupID: tc.accounts},
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, ModelsListConfig: tc.customList},
			})
			h.Models(c)
			require.Equal(t, http.StatusOK, rec.Code)
			var got gatewayModelsResponseForTest
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, "list", got.Object)
			require.NotNil(t, got.Data, "空列表必须是 JSON 数组而不是 null")
			ids := modelIDsForTest(got.Data)
			if tc.exact {
				require.Equal(t, tc.want, ids)
			} else {
				for _, id := range tc.want {
					require.Contains(t, ids, id)
				}
			}
			for _, model := range got.Data {
				require.Equal(t, "model", model.Object, "模型 %s", model.ID)
				require.Positive(t, model.Created, "模型 %s", model.ID)
				require.Equal(t, "openai", model.OwnedBy, "模型 %s", model.ID)
				require.Empty(t, model.CreatedAt, "模型项应使用 created 而非 created_at")
				// 内置模型沿用已有元数据，自定义别名继续使用既有回退值。
				for _, predefined := range openai.DefaultModels {
					if predefined.ID == model.ID {
						require.Equal(t, predefined.Created, model.Created)
						require.Equal(t, predefined.DisplayName, model.DisplayName)
					}
				}
			}
		})
	}
}

func TestGatewayModels_OpenAICodexContextMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(7042)
	h := newGatewayModelsHandlerForTest(&gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{groupID: {{
			ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		}}},
	})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI},
	})

	h.Models(c)
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Data []openai.Model `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	var model *openai.Model
	for i := range got.Data {
		if got.Data[i].ID == "gpt-5.6-sol" {
			model = &got.Data[i]
			break
		}
	}
	require.NotNil(t, model)
	require.EqualValues(t, 272000, model.ContextWindow)
	require.EqualValues(t, 872000, model.MaxContextWindow)
	require.EqualValues(t, 95, model.EffectiveContextWindowPercent)
}
