package server_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/ent/enttest"
	adminhandler "github.com/TokenFlux/TokenRouter/internal/handler/admin"
	"github.com/TokenFlux/TokenRouter/internal/model"
	"github.com/TokenFlux/TokenRouter/internal/repository"
	"github.com/TokenFlux/TokenRouter/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// 使用本地内存数据库覆盖 HTTP→服务→Ent→回读及运行时缓存，不连接生产数据库。
func TestTLSFingerprintProfileNativeOrderCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sql.Open("sqlite", "file:tls-native-order-crud?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	defer func() { _ = client.Close() }()
	repo := repository.NewTLSFingerprintProfileRepository(client)
	svc := service.NewTLSFingerprintProfileService(repo, nil)
	handler := adminhandler.NewTLSFingerprintProfileHandler(svc)
	router := gin.New()
	router.POST("/profiles", handler.Create)
	router.PUT("/profiles/:id", handler.Update)
	router.GET("/profiles/:id", handler.GetByID)

	call := func(method, path, body string, status int) *model.TLSFingerprintProfile {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		require.Equal(t, status, response.Code, response.Body.String())
		if status != http.StatusOK {
			return nil
		}
		var result struct {
			Data model.TLSFingerprintProfile `json:"data"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		return &result.Data
	}

	legacy := call(http.MethodPost, "/profiles", `{"name":"legacy"}`, http.StatusOK)
	require.False(t, legacy.RustlsNativeOrder)
	native := call(http.MethodPost, "/profiles", `{"name":"native","rustls_native_order":true}`, http.StatusOK)
	require.True(t, native.RustlsNativeOrder)
	require.True(t, svc.GetProfileByID(native.ID).RustlsNativeOrder)
	path := fmt.Sprintf("/profiles/%d", native.ID)
	partial := call(http.MethodPut, path, `{"description":"keep ordering"}`, http.StatusOK)
	require.True(t, partial.RustlsNativeOrder, "旧客户端省略新字段时保留原值")
	require.True(t, call(http.MethodGet, path, "", http.StatusOK).RustlsNativeOrder)
	restarted := service.NewTLSFingerprintProfileService(repo, nil)
	require.True(t, restarted.GetProfileByID(native.ID).RustlsNativeOrder, "重建服务后从数据库恢复显式策略")
	disabled := call(http.MethodPut, path, `{"rustls_native_order":false}`, http.StatusOK)
	require.False(t, disabled.RustlsNativeOrder)
	require.False(t, svc.GetProfileByID(native.ID).RustlsNativeOrder)
	require.False(t, call(http.MethodGet, path, "", http.StatusOK).RustlsNativeOrder)
	call(http.MethodPut, path, `{"rustls_native_order":true,"enable_grease":true}`, http.StatusBadRequest)
	require.False(t, call(http.MethodGet, path, "", http.StatusOK).RustlsNativeOrder, "失败更新不能污染原策略")
}
