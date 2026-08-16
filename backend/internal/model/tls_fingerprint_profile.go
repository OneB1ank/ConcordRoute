// Package model 定义服务层使用的数据模型。
package model

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/TokenFlux/TokenRouter/internal/pkg/tlsfingerprint"
)

// TLSFingerprintProfile TLS 指纹配置模板
// 包含完整的 ClientHello 参数，用于模拟特定客户端的 TLS 握手特征
type TLSFingerprintProfile struct {
	ID                  int64     `json:"id"`
	Name                string    `json:"name"`
	Description         *string   `json:"description"`
	EnableGREASE        bool      `json:"enable_grease"`
	CipherSuites        []uint16  `json:"cipher_suites"`
	Curves              []uint16  `json:"curves"`
	PointFormats        []uint16  `json:"point_formats"`
	SignatureAlgorithms []uint16  `json:"signature_algorithms"`
	ALPNProtocols       []string  `json:"alpn_protocols"`
	SupportedVersions   []uint16  `json:"supported_versions"`
	KeyShareGroups      []uint16  `json:"key_share_groups"`
	PSKModes            []uint16  `json:"psk_modes"`
	Extensions          []uint16  `json:"extensions"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// Validate 验证模板配置的有效性
func (p *TLSFingerprintProfile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}

	for _, field := range []struct {
		name   string
		values []uint16
	}{
		{name: "cipher_suites", values: p.CipherSuites},
		{name: "curves", values: p.Curves},
		{name: "point_formats", values: p.PointFormats},
		{name: "signature_algorithms", values: p.SignatureAlgorithms},
		{name: "supported_versions", values: p.SupportedVersions},
		{name: "key_share_groups", values: p.KeyShareGroups},
		{name: "psk_modes", values: p.PSKModes},
		{name: "extensions", values: p.Extensions},
	} {
		if duplicate, ok := duplicateUint16(field.values); ok {
			return &ValidationError{Field: field.name, Message: fmt.Sprintf("duplicate value %d", duplicate)}
		}
	}

	if invalid, ok := firstUint8Overflow(p.PointFormats); ok {
		return &ValidationError{Field: "point_formats", Message: fmt.Sprintf("value %d must be between 0 and 255", invalid)}
	}
	if invalid, ok := firstUint8Overflow(p.PSKModes); ok {
		return &ValidationError{Field: "psk_modes", Message: fmt.Sprintf("value %d must be between 0 and 255", invalid)}
	}

	seenALPN := make(map[string]struct{}, len(p.ALPNProtocols))
	for _, protocol := range p.ALPNProtocols {
		trimmed := strings.TrimSpace(protocol)
		if trimmed == "" {
			return &ValidationError{Field: "alpn_protocols", Message: "protocol must not be empty"}
		}
		if trimmed != protocol {
			return &ValidationError{Field: "alpn_protocols", Message: fmt.Sprintf("protocol %q must not contain surrounding whitespace", protocol)}
		}
		if size := len([]byte(protocol)); size > 255 {
			return &ValidationError{Field: "alpn_protocols", Message: fmt.Sprintf("protocol %q exceeds 255 bytes", protocol)}
		}
		if strings.IndexFunc(protocol, unicode.IsControl) >= 0 {
			return &ValidationError{Field: "alpn_protocols", Message: fmt.Sprintf("protocol %q contains control characters", protocol)}
		}
		if _, exists := seenALPN[protocol]; exists {
			return &ValidationError{Field: "alpn_protocols", Message: fmt.Sprintf("duplicate protocol %q", protocol)}
		}
		seenALPN[protocol] = struct{}{}
	}

	for _, version := range p.SupportedVersions {
		if version < 0x0301 || version > 0x0304 {
			return &ValidationError{Field: "supported_versions", Message: fmt.Sprintf("unsupported TLS version 0x%04x", version)}
		}
	}

	if len(p.Extensions) > 0 {
		extensions := make(map[uint16]struct{}, len(p.Extensions))
		for _, extension := range p.Extensions {
			extensions[extension] = struct{}{}
		}
		for _, relation := range []struct {
			field     string
			populated bool
			extension uint16
		}{
			{field: "curves", populated: len(p.Curves) > 0, extension: 10},
			{field: "point_formats", populated: len(p.PointFormats) > 0, extension: 11},
			{field: "signature_algorithms", populated: len(p.SignatureAlgorithms) > 0, extension: 13},
			{field: "alpn_protocols", populated: len(p.ALPNProtocols) > 0, extension: 16},
			{field: "supported_versions", populated: len(p.SupportedVersions) > 0, extension: 43},
			{field: "psk_modes", populated: len(p.PSKModes) > 0, extension: 45},
			{field: "key_share_groups", populated: len(p.KeyShareGroups) > 0, extension: 51},
		} {
			if !relation.populated {
				continue
			}
			if _, exists := extensions[relation.extension]; !exists {
				return &ValidationError{
					Field:   "extensions",
					Message: fmt.Sprintf("extension %d is required when %s is configured", relation.extension, relation.field),
				}
			}
		}

		// supported_versions 留空时，运行时会使用 TLS 1.3 + TLS 1.2 默认值。
		// 自定义扩展显式包含 43 时，同样要按 TLS 1.3 ClientHello 校验依赖，
		// 避免配置保存成功后才在握手阶段暴露缺少 key_share/signature_algorithms。
		tls13Enabled := containsUint16(p.SupportedVersions, 0x0304)
		if len(p.SupportedVersions) == 0 {
			_, tls13Enabled = extensions[43]
		}
		if tls13Enabled {
			for _, required := range []uint16{43, 51, 13} {
				if _, exists := extensions[required]; !exists {
					return &ValidationError{
						Field:   "extensions",
						Message: fmt.Sprintf("extension %d is required for TLS 1.3", required),
					}
				}
			}
		}
	}
	return nil
}

// duplicateUint16 返回首个重复值，避免配置直到握手阶段才暴露歧义。
func duplicateUint16(values []uint16) (uint16, bool) {
	seen := make(map[uint16]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return value, true
		}
		seen[value] = struct{}{}
	}
	return 0, false
}

// firstUint8Overflow 检查运行时会转换为 uint8 的字段，阻止静默截断。
func firstUint8Overflow(values []uint16) (uint16, bool) {
	for _, value := range values {
		if value > 255 {
			return value, true
		}
	}
	return 0, false
}

func containsUint16(values []uint16, target uint16) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// ToTLSProfile 将领域模型转换为运行时使用的 tlsfingerprint.Profile
// 空切片字段会在 dialer 中 fallback 到内置默认值
func (p *TLSFingerprintProfile) ToTLSProfile() *tlsfingerprint.Profile {
	return &tlsfingerprint.Profile{
		Name:                p.Name,
		EnableGREASE:        p.EnableGREASE,
		CipherSuites:        p.CipherSuites,
		Curves:              p.Curves,
		PointFormats:        p.PointFormats,
		SignatureAlgorithms: p.SignatureAlgorithms,
		ALPNProtocols:       p.ALPNProtocols,
		SupportedVersions:   p.SupportedVersions,
		KeyShareGroups:      p.KeyShareGroups,
		PSKModes:            p.PSKModes,
		Extensions:          p.Extensions,
	}
}
