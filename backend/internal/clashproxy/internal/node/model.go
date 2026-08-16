package node

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

type Type string

const (
	TypeHTTP      Type = "http"
	TypeHTTPS     Type = "https"
	TypeSOCKS5    Type = "socks5"
	TypeSS        Type = "shadowsocks"
	TypeVMess     Type = "vmess"
	TypeVLESS     Type = "vless"
	TypeTrojan    Type = "trojan"
	TypeHysteria  Type = "hysteria2"
	TypeTUIC      Type = "tuic"
	TypeSnell     Type = "snell"
	TypeWireGuard Type = "wireguard"
	TypeSSH       Type = "ssh"
)

type SourceType string

const (
	SourceManual            SourceType = "manual"
	SourceClashSubscription SourceType = "clash_subscription"
	SourceURI               SourceType = "uri"
	SourceMigrated          SourceType = "migrated"
)

type Node struct {
	ID         int64
	Name       string
	Type       Type
	SourceType SourceType
	Config     map[string]any
	Secret     map[string]any
}

func InternalName(id int64) string {
	return fmt.Sprintf("tr-clash-node-%d", id)
}

func IsAllowedType(t Type) bool {
	switch t {
	case TypeHTTP, TypeHTTPS, TypeSOCKS5, TypeSS, TypeVMess, TypeVLESS, TypeTrojan, TypeHysteria, TypeTUIC, TypeSnell, TypeWireGuard, TypeSSH:
		return true
	default:
		return false
	}
}

func ManualURL(name, rawURL string) (*Node, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if parsed == nil || strings.TrimSpace(parsed.Scheme) == "" {
		return nil, errors.New("proxy URL scheme is required")
	}
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return nil, errors.New("proxy URL host is required")
	}
	portText := strings.TrimSpace(parsed.Port())
	if portText == "" {
		return nil, errors.New("proxy URL port is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("proxy URL port is invalid")
	}
	nodeType := Type(strings.ToLower(strings.TrimSpace(parsed.Scheme)))
	if nodeType == "socks5h" {
		nodeType = TypeSOCKS5
	}
	if !IsAllowedType(nodeType) {
		return nil, fmt.Errorf("unsupported node type %q", parsed.Scheme)
	}

	config := map[string]any{"server": host, "port": port}
	if ip := net.ParseIP(host); ip != nil {
		config["server"] = ip.String()
	}
	secret := map[string]any{}
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			secret["username"] = username
		}
		if password, ok := parsed.User.Password(); ok {
			secret["password"] = password
		}
	}
	return &Node{
		Name:       strings.TrimSpace(name),
		Type:       nodeType,
		SourceType: SourceManual,
		Config:     config,
		Secret:     secret,
	}, nil
}
