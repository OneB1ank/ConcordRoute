package mihomo

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	proxynode "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/node"
	proxyprofile "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/profile"
	"gopkg.in/yaml.v3"
)

type Profile struct {
	ID              int64
	Strategy        proxyprofile.Strategy
	TestURL         string
	IntervalSeconds int
}

type RuntimeConfig struct {
	MixedPort        int
	ControllerPort   int
	ControllerSecret string
}

func Compile(profile Profile, nodes []proxynode.Node, runtime RuntimeConfig) ([]byte, error) {
	if profile.ID <= 0 {
		return nil, errors.New("profile id is required")
	}
	strategy := normalizeStrategy(profile.Strategy)
	if strategy == "" {
		return nil, fmt.Errorf("unsupported proxy profile strategy %q", profile.Strategy)
	}
	if runtime.MixedPort <= 0 || runtime.ControllerPort <= 0 {
		return nil, errors.New("runtime ports are required")
	}
	if strings.TrimSpace(runtime.ControllerSecret) == "" {
		return nil, errors.New("controller secret is required")
	}
	if len(nodes) == 0 {
		return nil, errors.New("at least one proxy node is required")
	}
	testURL := strings.TrimSpace(profile.TestURL)
	if testURL == "" {
		testURL = "https://www.gstatic.com/generate_204"
	}
	interval := profile.IntervalSeconds
	if interval <= 0 {
		interval = 300
	}

	proxies := make([]map[string]any, 0, len(nodes))
	proxyNames := make([]string, 0, len(nodes))
	for _, n := range nodes {
		compiled, err := compileNode(n)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, compiled)
		if name, ok := compiled["name"].(string); ok {
			proxyNames = append(proxyNames, name)
		}
	}

	groupName := fmt.Sprintf("TR-CLASH-%d", profile.ID)
	doc := map[string]any{
		"mixed-port":          runtime.MixedPort,
		"allow-lan":           false,
		"bind-address":        "127.0.0.1",
		"mode":                "rule",
		"log-level":           "warning",
		"ipv6":                false,
		"find-process-mode":   "off",
		"unified-delay":       true,
		"tcp-concurrent":      true,
		"external-controller": fmt.Sprintf("127.0.0.1:%d", runtime.ControllerPort),
		"secret":              runtime.ControllerSecret,
		"profile": map[string]any{
			"store-selected": true,
			"store-fake-ip":  false,
		},
		"dns":     map[string]any{"enable": false},
		"proxies": proxies,
		"proxy-groups": []map[string]any{{
			"name":     groupName,
			"type":     strategy,
			"proxies":  proxyNames,
			"url":      testURL,
			"interval": interval,
		}},
		"rules": []string{"MATCH," + groupName},
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func normalizeStrategy(strategy proxyprofile.Strategy) string {
	switch strategy {
	case "", proxyprofile.StrategySelect:
		return "select"
	case proxyprofile.StrategyURLTest:
		return "url-test"
	case proxyprofile.StrategyFallback:
		return "fallback"
	case proxyprofile.StrategyLoadBalance:
		return "load-balance"
	default:
		return ""
	}
}

func compileNode(n proxynode.Node) (map[string]any, error) {
	if n.ID <= 0 {
		return nil, errors.New("proxy node id is required")
	}
	if !proxynode.IsAllowedType(n.Type) {
		return nil, fmt.Errorf("unsupported proxy node type %q", n.Type)
	}
	server, ok := stringValue(n.Config, "server")
	if !ok {
		return nil, errors.New("proxy node server is required")
	}
	port, ok := intValue(n.Config, "port")
	if !ok || port <= 0 || port > 65535 {
		return nil, errors.New("proxy node port is invalid")
	}

	nodeType := normalizeNodeType(n.Type)
	if nodeType == "" {
		return nil, fmt.Errorf("unsupported proxy node type %q", n.Type)
	}
	out := map[string]any{
		"name":   proxynode.InternalName(n.ID),
		"type":   nodeType,
		"server": server,
		"port":   port,
	}
	for key, value := range n.Config {
		switch key {
		case "name", "type", "server", "port":
			continue
		default:
			out[key] = value
		}
	}
	for key, value := range n.Secret {
		switch key {
		case "name", "type", "server", "port":
			continue
		default:
			out[key] = value
		}
	}
	if n.Type == proxynode.TypeHTTPS {
		out["tls"] = true
	}
	return out, nil
}

func normalizeNodeType(nodeType proxynode.Type) string {
	switch nodeType {
	case proxynode.TypeHTTPS:
		return "http"
	case proxynode.TypeSS:
		return "ss"
	default:
		return string(nodeType)
	}
}

func stringValue(values map[string]any, key string) (string, bool) {
	if values == nil {
		return "", false
	}
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	return text, text != ""
}

func intValue(values map[string]any, key string) (int, bool) {
	if values == nil {
		return 0, false
	}
	switch v := values[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		return i, err == nil
	default:
		return 0, false
	}
}
