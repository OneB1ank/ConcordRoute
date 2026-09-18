package config

import (
	"errors"
	"os"
	"strings"
)

// PluginPreviewBuild is set only by the isolated desktop preview packager.
// Unlike an environment-only switch it cannot be accidentally unset at launch.
var PluginPreviewBuild string

// Public verification material for the companion local account-protection
// package. The signing secret is never embedded in the preview binary.
const (
	PluginPreviewPublisherKeyID     = "local-account-protection-v1"
	PluginPreviewPublisherKeyBase64 = "9jp4x3g0CBBD2Vks9t7EFsuwEcZjICg+Ib3sYW/i+YQ="
)

func PreviewTrustedPublishers() map[string]string {
	return map[string]string{PluginPreviewPublisherKeyID: PluginPreviewPublisherKeyBase64}
}

func PluginPreviewEnabled() bool {
	return PluginPreviewBuild == "1" || os.Getenv("SUB2API_PLUGIN_PREVIEW") == "1"
}
func previewLoopback(host string) bool {
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost") || host == "::1"
}
func ValidatePluginPreviewDatabase(host string, port int, database string) error {
	if PluginPreviewEnabled() && (!previewLoopback(host) || port != 25432 || database != "sub2api_plugin_preview") {
		return errors.New("插件测试版只允许本机 25432 端口的 sub2api_plugin_preview 数据库；不能连接正式库，请使用独立快速安装")
	}
	return nil
}
func ValidatePluginPreviewRedis(host string, port int) error {
	if PluginPreviewEnabled() && (!previewLoopback(host) || port != 26379) {
		return errors.New("插件测试版只允许本机 26379 端口的独立缓存；不能连接正式缓存")
	}
	return nil
}
