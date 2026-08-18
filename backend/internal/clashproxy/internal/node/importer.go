package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

func ImportClashYAML(content []byte) ([]Node, error) {
	if len(content) == 0 {
		return nil, errors.New("clash YAML content is required")
	}
	var root struct {
		Proxies        []map[string]any         `yaml:"proxies"`
		ProxyProviders map[string]clashProvider `yaml:"proxy-providers"`
	}
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("parse clash YAML: %w", err)
	}
	rawProxies := append([]map[string]any{}, root.Proxies...)
	for _, provider := range root.ProxyProviders {
		rawProxies = append(rawProxies, provider.Proxies...)
	}
	nodes := make([]Node, 0, len(rawProxies))
	usedNames := map[string]int{}
	for _, raw := range rawProxies {
		node, err := normalizeClashProxy(raw)
		if err != nil {
			return nil, err
		}
		node.Name = dedupeName(node.Name, usedNames)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

type clashProvider struct {
	Proxies []map[string]any `yaml:"proxies"`
}

func ImportURI(rawURI string) (*Node, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URI: %w", err)
	}
	if parsed == nil {
		return nil, errors.New("proxy URI is required")
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "http", "https", "socks5", "socks5h":
		node, err := ManualURL(fragmentName(parsed), rawURI)
		if err != nil {
			return nil, err
		}
		node.SourceType = SourceURI
		return node, nil
	case "ss":
		return importShadowsocksURI(rawURI, parsed)
	case "vmess":
		return importVMessURI(rawURI)
	case "vless":
		return importCredentialURI(parsed, TypeVLESS, "uuid")
	case "trojan":
		return importCredentialURI(parsed, TypeTrojan, "password")
	case "hysteria2", "hy2":
		return importCredentialURI(parsed, TypeHysteria, "password")
	case "tuic":
		return importTUICURI(parsed)
	default:
		return nil, fmt.Errorf("unsupported proxy URI scheme %q", parsed.Scheme)
	}
}

func importCredentialURI(parsed *url.URL, nodeType Type, secretKey string) (*Node, error) {
	config, err := configFromParsedURI(parsed)
	if err != nil {
		return nil, err
	}
	applyCredentialURIOptions(config, parsed, nodeType)
	secret := map[string]any{}
	if parsed.User != nil {
		value := parsed.User.Username()
		if value != "" {
			secret[secretKey] = value
		}
		if password, ok := parsed.User.Password(); ok && password != "" {
			secret["password"] = password
		}
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("%s URI credential is required", nodeType)
	}
	return &Node{
		Name:       fragmentName(parsed),
		Type:       nodeType,
		SourceType: SourceURI,
		Config:     config,
		Secret:     secret,
	}, nil
}

func applyCredentialURIOptions(config map[string]any, parsed *url.URL, nodeType Type) {
	query := parsed.Query()
	if network := firstNonEmpty(query.Get("network"), query.Get("type")); network != "" {
		config["network"] = network
	}
	if encryption := strings.TrimSpace(query.Get("encryption")); encryption != "" {
		config["encryption"] = encryption
	}
	security := strings.ToLower(strings.TrimSpace(query.Get("security")))
	if security == "tls" || security == "reality" {
		config["tls"] = true
	}
	if nodeType != TypeVLESS {
		return
	}
	if sni := strings.TrimSpace(query.Get("sni")); sni != "" {
		delete(config, "sni")
		config["servername"] = sni
	}
	if fingerprint := firstNonEmpty(query.Get("client-fingerprint"), query.Get("fp")); fingerprint != "" {
		config["client-fingerprint"] = fingerprint
	}
	if security == "reality" {
		reality := map[string]any{}
		if publicKey := strings.TrimSpace(query.Get("pbk")); publicKey != "" {
			reality["public-key"] = publicKey
		}
		if shortID := strings.TrimSpace(query.Get("sid")); shortID != "" {
			reality["short-id"] = shortID
		}
		if len(reality) > 0 {
			config["reality-opts"] = reality
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func importTUICURI(parsed *url.URL) (*Node, error) {
	node, err := importCredentialURI(parsed, TypeTUIC, "uuid")
	if err != nil {
		return nil, err
	}
	if parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			node.Secret["password"] = password
		}
	}
	return node, nil
}

func importShadowsocksURI(rawURI string, parsed *url.URL) (*Node, error) {
	if parsed.Hostname() == "" && parsed.Opaque != "" {
		decoded, err := decodeBase64Text(strings.TrimSpace(parsed.Opaque))
		if err != nil {
			return nil, fmt.Errorf("invalid shadowsocks URI: %w", err)
		}
		return ImportURI("ss://" + decoded)
	}
	config, err := configFromParsedURI(parsed)
	if err != nil {
		return nil, err
	}
	secret := map[string]any{}
	if parsed.User != nil {
		username := parsed.User.Username()
		password, hasPassword := parsed.User.Password()
		if !hasPassword {
			if decoded, err := decodeBase64Text(username); err == nil {
				if method, pass, ok := strings.Cut(decoded, ":"); ok {
					username = method
					password = pass
					hasPassword = true
				}
			}
		}
		if username != "" {
			secret["cipher"] = username
		}
		if hasPassword && password != "" {
			secret["password"] = password
		}
	}
	if secret["cipher"] == nil || secret["password"] == nil {
		return nil, fmt.Errorf("invalid shadowsocks URI: %s", rawURI)
	}
	return &Node{
		Name:       fragmentName(parsed),
		Type:       TypeSS,
		SourceType: SourceURI,
		Config:     config,
		Secret:     secret,
	}, nil
}

func importVMessURI(rawURI string) (*Node, error) {
	payload := strings.TrimSpace(strings.TrimPrefix(rawURI, "vmess://"))
	if idx := strings.IndexAny(payload, "?#"); idx >= 0 {
		payload = payload[:idx]
	}
	decoded, err := decodeBase64Text(payload)
	if err != nil {
		return nil, fmt.Errorf("invalid vmess URI: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(decoded), &raw); err != nil {
		return nil, fmt.Errorf("parse vmess URI: %w", err)
	}
	server := stringFrom(raw, "add")
	if server == "" {
		return nil, errors.New("vmess server is required")
	}
	port, ok := intFrom(raw, "port")
	if !ok || port <= 0 || port > 65535 {
		return nil, errors.New("vmess port is invalid")
	}
	config := map[string]any{"server": server, "port": port}
	copyStringField(config, raw, "net", "network")
	copyStringField(config, raw, "tls", "tls")
	copyStringField(config, raw, "sni", "sni")
	copyStringField(config, raw, "host", "host")
	copyStringField(config, raw, "path", "path")
	secret := map[string]any{}
	copyStringField(secret, raw, "id", "uuid")
	copyValueField(secret, raw, "aid", "alterId")
	copyStringField(secret, raw, "scy", "cipher")
	if secret["uuid"] == nil {
		return nil, errors.New("vmess uuid is required")
	}
	return &Node{
		Name:       stringFrom(raw, "ps"),
		Type:       TypeVMess,
		SourceType: SourceURI,
		Config:     config,
		Secret:     secret,
	}, nil
}

func configFromParsedURI(parsed *url.URL) (map[string]any, error) {
	host := strings.TrimSpace(parsed.Hostname())
	if host == "" {
		return nil, errors.New("proxy URI host is required")
	}
	port, err := strconv.Atoi(strings.TrimSpace(parsed.Port()))
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("proxy URI port is invalid")
	}
	config := map[string]any{"server": host, "port": port}
	query := parsed.Query()
	for _, key := range []string{"network", "tls", "sni", "alpn", "client-fingerprint", "flow"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			config[key] = value
		}
	}
	for _, key := range []string{"udp", "skip-cert-verify"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			config[key] = value == "true" || value == "1"
		}
	}
	return config, nil
}

func fragmentName(parsed *url.URL) string {
	name, err := url.QueryUnescape(strings.TrimSpace(parsed.Fragment))
	if err != nil {
		return strings.TrimSpace(parsed.Fragment)
	}
	return strings.TrimSpace(name)
}

func decodeBase64Text(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("base64 payload is required")
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := encoding.DecodeString(value)
		if err == nil {
			return string(decoded), nil
		}
	}
	return "", errors.New("base64 decode failed")
}

func copyStringField(dst map[string]any, src map[string]any, from string, to string) {
	if value := stringFrom(src, from); value != "" {
		dst[to] = value
	}
}

func copyValueField(dst map[string]any, src map[string]any, from string, to string) {
	if value, ok := src[from]; ok {
		dst[to] = value
	}
}

func normalizeClashProxy(raw map[string]any) (Node, error) {
	name := stringFrom(raw, "name")
	nodeType := Type(strings.ToLower(stringFrom(raw, "type")))
	if nodeType == "socks5h" {
		nodeType = TypeSOCKS5
	}
	if !IsAllowedType(nodeType) {
		return Node{}, fmt.Errorf("unsupported clash proxy type %q", nodeType)
	}
	server := stringFrom(raw, "server")
	if server == "" {
		return Node{}, errors.New("clash proxy server is required")
	}
	port, ok := intFrom(raw, "port")
	if !ok || port <= 0 || port > 65535 {
		return Node{}, errors.New("clash proxy port is invalid")
	}

	config := make(map[string]any, len(raw))
	secret := map[string]any{}
	secretKeys := map[string]struct{}{}
	for _, key := range []string{
		"username",
		"password",
		"uuid",
		"alterId",
		"cipher",
		"token",
		"auth-str",
		"private-key",
		"psk",
		"client-key",
		"passphrase",
	} {
		secretKeys[key] = struct{}{}
		if value, ok := raw[key]; ok {
			secret[key] = value
		}
	}
	for key, value := range raw {
		if _, secretField := secretKeys[key]; secretField {
			continue
		}
		switch key {
		case "name", "type":
			continue
		default:
			config[key] = value
		}
	}
	config["server"] = server
	config["port"] = port
	return Node{
		Name:       name,
		Type:       nodeType,
		SourceType: SourceClashSubscription,
		Config:     config,
		Secret:     secret,
	}, nil
}

func dedupeName(name string, used map[string]int) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Imported Proxy"
	}
	used[name]++
	if used[name] == 1 {
		return name
	}
	return fmt.Sprintf("%s (%d)", name, used[name])
}

func stringFrom(raw map[string]any, key string) string {
	if raw == nil {
		return ""
	}
	value, ok := raw[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intFrom(raw map[string]any, key string) (int, bool) {
	if raw == nil {
		return 0, false
	}
	switch value := raw[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		return parsed, err == nil
	default:
		return 0, false
	}
}
