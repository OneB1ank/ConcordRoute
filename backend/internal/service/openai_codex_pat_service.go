package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/errors"
	"github.com/TokenFlux/TokenRouter/internal/pkg/httpclient"
)

const openAICodexPATWhoamiURLDefault = "https://auth.openai.com/api/accounts/v1/user-auth-credential/whoami"

var openAICodexPATWhoamiURL = openAICodexPATWhoamiURLDefault

var openAIPersonalAccessTokenOAuthCredentialKeys = [...]string{
	"refresh_token",
	"id_token",
	"expires_at",
	"expires_in",
	"client_id",
}

type openAICodexPATWhoamiResponse struct {
	Email                   string `json:"email"`
	ChatGPTUserID           string `json:"chatgpt_user_id"`
	ChatGPTAccountID        string `json:"chatgpt_account_id"`
	ChatGPTPlanType         string `json:"chatgpt_plan_type"`
	ChatGPTAccountIsFedRAMP *bool  `json:"chatgpt_account_is_fedramp"`
}

type OpenAICodexPATValidationOptions struct {
	TLSFingerprintRouterID int64
	Account                *Account
}

// ValidateCodexPersonalAccessToken 使用 Codex 官方 PAT whoami 端点校验 at-* token。
func (s *OpenAIOAuthService) ValidateCodexPersonalAccessToken(ctx context.Context, accessToken, proxyURL string, validationOptions ...OpenAICodexPATValidationOptions) (*OpenAITokenInfo, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_PAT_REQUIRED", "access token is required")
	}
	if !strings.HasPrefix(accessToken, "at-") {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_PAT_INVALID_PREFIX", "Codex personal access token must start with at-")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAICodexPATWhoamiURL, nil)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_CODEX_PAT_REQUEST_FAILED", "failed to build validation request: %v", err)
	}
	req.Header.Set("authorization", "Bearer "+accessToken)
	req.Header.Set("accept", "application/json")
	ApplyCodexCanonicalAuthIdentity(req.Header)

	requestOption := s.resolveCodexPATRequestOption(ctx, validationOptions...)
	if strings.TrimSpace(requestOption.UserAgent) != "" {
		userAgent, originator := CodexAuthIdentityForUserAgent(requestOption.UserAgent)
		req.Header.Set("user-agent", userAgent)
		req.Header.Set("originator", originator)
		req.Header.Del("version")
	}

	var resp *http.Response
	if s != nil && s.httpUpstream != nil {
		if requestOption.TLSProfile != nil {
			resp, err = s.httpUpstream.DoWithTLS(req, proxyURL, requestOption.AccountID, requestOption.AccountConcurrency, requestOption.TLSProfile)
		} else {
			resp, err = s.httpUpstream.Do(req, proxyURL, requestOption.AccountID, requestOption.AccountConcurrency)
		}
	} else {
		var client *http.Client
		client, err = httpclient.GetClient(httpclient.Options{
			ProxyURL:              proxyURL,
			Timeout:               20 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		})
		if err == nil {
			resp, err = client.Do(req)
		}
	}
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_PAT_VALIDATE_FAILED", "failed to validate Codex personal access token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_CODEX_PAT_INVALID", "Codex personal access token is invalid or expired")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_PAT_VALIDATE_FAILED", "Codex personal access token validation failed: %s", message)
	}

	var whoami openAICodexPATWhoamiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&whoami); err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_PAT_RESPONSE_INVALID", "invalid Codex personal access token validation response: %v", err)
	}
	if err := validateOpenAICodexPATWhoami(whoami); err != nil {
		return nil, err
	}

	return &OpenAITokenInfo{
		AccessToken:           accessToken,
		AuthMode:              OpenAIAuthModePersonalAccessToken,
		Email:                 strings.TrimSpace(whoami.Email),
		ChatGPTAccountID:      strings.TrimSpace(whoami.ChatGPTAccountID),
		ChatGPTUserID:         strings.TrimSpace(whoami.ChatGPTUserID),
		ChatGPTAccountFedRAMP: *whoami.ChatGPTAccountIsFedRAMP,
		PlanType:              strings.TrimSpace(whoami.ChatGPTPlanType),
	}, nil
}

func (s *OpenAIOAuthService) resolveCodexPATRequestOption(ctx context.Context, options ...OpenAICodexPATValidationOptions) OpenAIOAuthTokenRequestOptions {
	if len(options) == 0 {
		return OpenAIOAuthTokenRequestOptions{}
	}
	input := options[0]
	routerID := input.TLSFingerprintRouterID
	if routerID <= 0 && input.Account != nil {
		routerID = input.Account.GetTLSFingerprintRouterID()
	}
	resolved := s.resolveChatGPTOAuthTokenRequestOptions(ctx, routerID, input.Account)
	if len(resolved) == 0 {
		return OpenAIOAuthTokenRequestOptions{}
	}
	return resolved[0]
}

func validateOpenAICodexPATWhoami(whoami openAICodexPATWhoamiResponse) error {
	required := map[string]string{
		"email":              whoami.Email,
		"chatgpt_user_id":    whoami.ChatGPTUserID,
		"chatgpt_account_id": whoami.ChatGPTAccountID,
		"chatgpt_plan_type":  whoami.ChatGPTPlanType,
	}
	for key, value := range required {
		if strings.TrimSpace(value) == "" {
			return infraerrors.Newf(http.StatusBadGateway, "OPENAI_CODEX_PAT_RESPONSE_INVALID", "Codex personal access token validation response is missing %s", key)
		}
	}
	if whoami.ChatGPTAccountIsFedRAMP == nil {
		return infraerrors.New(http.StatusBadGateway, "OPENAI_CODEX_PAT_RESPONSE_INVALID", "Codex personal access token validation response is missing chatgpt_account_is_fedramp")
	}
	return nil
}

// NormalizeOpenAIPersonalAccessTokenCredentials 移除 PAT 账号中只属于 OAuth 刷新的凭证字段。
func NormalizeOpenAIPersonalAccessTokenCredentials(account *Account, tokenInfo *OpenAITokenInfo, credentials map[string]any) map[string]any {
	if credentials == nil || !isOpenAIPersonalAccessTokenCredentialSet(account, tokenInfo, credentials) {
		return credentials
	}

	for _, key := range openAIPersonalAccessTokenOAuthCredentialKeys {
		delete(credentials, key)
	}
	credentials[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
	credentials[openAIAuthModeLegacyCredentialKey] = "personal_access_token"
	credentials["token_type"] = "Bearer"
	return credentials
}

func isOpenAIPersonalAccessTokenCredentialSet(account *Account, tokenInfo *OpenAITokenInfo, credentials map[string]any) bool {
	if tokenInfo != nil && isOpenAIPersonalAccessTokenAuthMode(tokenInfo.AuthMode) {
		return true
	}
	if account != nil && account.IsOpenAIPersonalAccessToken() {
		return true
	}
	return isOpenAIPersonalAccessTokenAuthMode(openAICredentialString(credentials[openAIAuthModeCredentialKey])) ||
		isOpenAIPersonalAccessTokenAuthMode(openAICredentialString(credentials[openAIAuthModeLegacyCredentialKey]))
}

func openAICredentialString(value any) string {
	if v, ok := value.(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
