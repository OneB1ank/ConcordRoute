package service

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAICompatMessagesBridgeContextKey = "openai_compat_messages_bridge"

// 仅识别历史请求中的桥接标签，当前网关不再生成或追加对应的 developer 提示。
const openAICompatClaudeCodeTodoGuardMarker = "<sub2api-claude-code-todo-guard>"

func isOpenAICompatMessagesBridgeBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	if bytes.Contains(body, []byte(openAICompatClaudeCodeTodoGuardMarker)) {
		return true
	}
	return isOpenAICompatMessagesBridgePromptCacheKey(gjson.GetBytes(body, "prompt_cache_key").String())
}

func isOpenAICompatMessagesBridgeRequestBody(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	if input, ok := reqBody["input"].([]any); ok && inputContainsText(input, openAICompatClaudeCodeTodoGuardMarker) {
		return true
	}
	return isOpenAICompatMessagesBridgePromptCacheKey(firstNonEmptyString(reqBody["prompt_cache_key"]))
}

func isOpenAICompatMessagesBridgePromptCacheKey(key string) bool {
	key = strings.TrimSpace(key)
	return strings.HasPrefix(key, "anthropic-metadata-") ||
		strings.HasPrefix(key, "anthropic-cache-") ||
		strings.HasPrefix(key, "anthropic-digest-")
}

func setOpenAICompatMessagesBridgeContext(c *gin.Context, enabled bool) {
	if c == nil || !enabled {
		return
	}
	c.Set(openAICompatMessagesBridgeContextKey, true)
}

func isOpenAICompatMessagesBridgeContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAICompatMessagesBridgeContextKey)
	if !ok {
		return false
	}
	enabled, ok := value.(bool)
	return ok && enabled
}

// inputContainsText 保留旧请求的只读识别逻辑，不修改客户端正文。
func inputContainsText(input []any, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	for _, item := range input {
		b, err := json.Marshal(item)
		if err == nil && strings.Contains(string(b), needle) {
			return true
		}
	}
	return false
}
