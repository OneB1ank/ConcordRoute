package service

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// maxPersistedSessionIDLength 将客户端会话标识限制在 usage_logs.session_id 的
// VARCHAR(255) 宽度内；超长值直接拒绝，避免不同标识因截断而混为一体。
const maxPersistedSessionIDLength = 255

// clientSessionIDHeaders 是用量日志持久化专用的识别顺序。它与调度亲和顺序
// 分离，避免任一链路后续调整请求头兼容策略时意外改变另一条链路的语义。
var clientSessionIDHeaders = []string{
	codexSessionIDHeader,
	"session_id",
	"conversation_id",
	openCodeSessionAffinityHeader,
	openCodeSessionIDHeader,
	openCodeNativeSessionHeader,
	codeBuddyConversationHeader,
	claudeCodeSessionHeader,
}

// ExtractClientSessionID 从请求头解析并清理客户端显式提供的会话标识，用于关联
// 用量日志。所有网关协议共用该入口，确保 session_id 记录口径一致；没有有效值时
// 返回空字符串。
//
// 该值只用于持久化 usage_logs.session_id，不影响粘性路由、账号选择、request_id
// 语义或上游提示缓存；这些功能继续使用各自更宽泛的会话信号解析规则。
func ExtractClientSessionID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	for _, header := range clientSessionIDHeaders {
		if sessionID := sanitizeSessionID(c.GetHeader(header)); sessionID != "" {
			return sessionID
		}
	}
	if isGrokRequestContext(c) {
		if sessionID := sanitizeSessionID(c.GetHeader(grokConversationIDHeader)); sessionID != "" {
			return sessionID
		}
	}
	return ""
}

// sanitizeSessionID 规范化客户端提供的原始会话标识：去除首尾空白，包含控制字符
// （CR、LF、制表符、NUL 等）或超过数据库列上限时整值拒绝，防止日志或请求头
// 注入内容进入关联数据。缺失或无效输入返回空字符串。
func sanitizeSessionID(raw string) string {
	if !utf8.ValidString(raw) {
		return ""
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	count := 0
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			// 显式关联标识不应包含控制字符；整值丢弃，避免持久化被篡改或部分注入的内容。
			return ""
		}
		count++
		if count > maxPersistedSessionIDLength {
			return ""
		}
	}
	return trimmed
}
