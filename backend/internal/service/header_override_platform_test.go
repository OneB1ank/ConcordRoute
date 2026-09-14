//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 保存与运行使用同一平台边界，保留旧配置和不同 map 形状的兼容性。
func TestHeaderOverridePlatformBoundary(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGrok} {
		for _, name := range []string{"Originator", "Version", "codex_version"} {
			t.Run(platform+"/"+name, func(t *testing.T) {
				creds := map[string]any{
					credKeyHeaderOverrideEnabled: true,
					credKeyHeaderOverrides:       map[string]any{name: "service-value"},
				}
				err := NormalizeHeaderOverrideCredentialsForPlatform(creds, platform)
				if platform == PlatformOpenAI {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				for _, raw := range []any{map[string]any{name: "service-value"}, map[string]string{name: "service-value"}} {
					a := &Account{Platform: platform, Type: AccountTypeAPIKey, Credentials: map[string]any{
						credKeyHeaderOverrideEnabled: true, credKeyHeaderOverrides: raw,
					}}
					for i := 0; i < 2; i++ {
						h := http.Header{}
						a.ApplyHeaderOverrides(h)
						if platform == PlatformOpenAI {
							require.Empty(t, getHeaderRaw(h, strings.ToLower(name)))
						} else {
							// 运行时以小写原始键写入；读取同一 wire key，避免 canonical 查询误报。
							require.Equal(t, "service-value", getHeaderRaw(h, strings.ToLower(name)))
						}
					}
				}
			})
		}
		for _, entry := range []map[string]any{
			{"Authorization": "value"}, {"Host": "value"}, {"Version": "bad\r\ninjected: value"},
		} {
			require.Error(t, NormalizeHeaderOverrideCredentialsForPlatform(map[string]any{
				credKeyHeaderOverrides: entry,
			}, platform))
		}
	}
	require.NoError(t, NormalizeHeaderOverrideCredentialsForPlatform(nil, PlatformOpenAI))
	// UA 仍允许保存兼容项，但 OpenAI 出站忽略它，不改变现有 UA 策略。
	require.NoError(t, NormalizeHeaderOverrideCredentialsForPlatform(map[string]any{
		credKeyHeaderOverrides: map[string]any{"User-Agent": "existing"},
	}, PlatformOpenAI))
}

// 混合平台批次在任何写操作前校验；非 OpenAI 批次正常保存通用版本头。
func TestHeaderOverrideBulkPlatformValidationBeforeWrite(t *testing.T) {
	for _, second := range []string{PlatformOpenAI, PlatformGrok} {
		t.Run(second, func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{
				{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
				{ID: 2, Platform: second, Type: AccountTypeAPIKey},
			}}
			svc := &adminServiceImpl{accountRepo: repo}
			_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{1, 2},
				Credentials: map[string]any{
					credKeyHeaderOverrideEnabled: true,
					credKeyHeaderOverrides:       map[string]any{"Version": "service-v2"},
				},
			})
			if second == PlatformOpenAI {
				require.Error(t, err)
				require.Empty(t, repo.bulkUpdateIDs)
			} else {
				require.NoError(t, err)
				require.ElementsMatch(t, []int64{1, 2}, repo.bulkUpdateIDs)
			}
		})
	}
}
