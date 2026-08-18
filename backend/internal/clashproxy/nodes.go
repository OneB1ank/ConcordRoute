package clashproxy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	proxynode "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/node"
)

func (s *Service) ListNodes(ctx context.Context) ([]NodeView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, node_type, source_type, config_json, status
FROM clash_proxy_nodes
WHERE deleted_at IS NULL
ORDER BY id DESC
`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]NodeView, 0)
	for rows.Next() {
		var item NodeView
		var configRaw []byte
		if err := rows.Scan(&item.ID, &item.Name, &item.NodeType, &item.SourceType, &configRaw, &item.Status); err != nil {
			return nil, err
		}
		if err := decodeJSONMap(configRaw, &item.Config); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) CreateManualNode(ctx context.Context, input CreateNodeInput) (*NodeView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	n, err := proxynode.ImportURI(input.URL)
	if err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(input.Name); name != "" {
		n.Name = name
	}
	id, err := s.insertNode(ctx, *n)
	if err != nil {
		return nil, err
	}
	return s.getNode(ctx, id)
}

func (s *Service) ImportNodes(ctx context.Context, input ImportNodesInput) ([]NodeView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	format := strings.ToLower(strings.TrimSpace(input.Format))
	content := strings.TrimSpace(input.Content)
	if strings.TrimSpace(input.URL) != "" {
		downloaded, err := proxynode.DownloadSubscription(ctx, input.URL, proxynode.SubscriptionDownloadOptions{
			MaxBytes:          s.options.SubscriptionMaxBytes,
			AllowInsecureHTTP: s.options.AllowInsecureSubscription,
			AllowPrivate:      s.options.AllowPrivateSubscription,
		})
		if err != nil {
			return nil, err
		}
		content = string(downloaded)
		if format == "" {
			format = "auto"
		}
	}
	if content == "" {
		return nil, errors.New("proxy import content or subscription URL is required")
	}

	nodes, err := parseImportedNodes(format, content)
	if err != nil {
		return nil, err
	}
	views := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		id, err := s.insertNode(ctx, n)
		if err != nil {
			return nil, err
		}
		view, err := s.getNode(ctx, id)
		if err != nil {
			return nil, err
		}
		views = append(views, *view)
	}
	return views, nil
}

func (s *Service) DeleteNode(ctx context.Context, id int64) error {
	if err := s.requireConfigured(); err != nil {
		return err
	}
	if id <= 0 {
		return errors.New("node id is required")
	}
	var references int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM clash_proxy_profile_nodes WHERE node_id = $1 AND enabled = TRUE
`, id).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return errors.New("proxy node is still used by a profile")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE clash_proxy_nodes
SET status = 'disabled', deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
`, id)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func parseImportedNodes(format, content string) ([]proxynode.Node, error) {
	switch format {
	case "", "auto", "subscription":
		if nodes, err := proxynode.ImportClashYAML([]byte(content)); err == nil && len(nodes) > 0 {
			return nodes, nil
		}
		return importURILines(content)
	case "clash_yaml", "clash", "yaml":
		return proxynode.ImportClashYAML([]byte(content))
	case "uri":
		return importURILines(content)
	default:
		return nil, fmt.Errorf("unsupported proxy import format %q", format)
	}
}

func importURILines(content string) ([]proxynode.Node, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	nodes := make([]proxynode.Node, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, err := proxynode.ImportURI(line)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, *n)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("proxy import contains no supported nodes")
	}
	return nodes, nil
}

func (s *Service) insertNode(ctx context.Context, n proxynode.Node) (int64, error) {
	configRaw, err := json.Marshal(n.Config)
	if err != nil {
		return 0, err
	}
	secretRaw, err := json.Marshal(n.Secret)
	if err != nil {
		return 0, err
	}
	name := strings.TrimSpace(n.Name)
	if name == "" {
		name = "Imported Proxy"
	}
	var id int64
	err = s.db.QueryRowContext(ctx, `
INSERT INTO clash_proxy_nodes (name, node_type, source_type, config_json, secret_json, status)
VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, 'active')
RETURNING id
`, name, string(n.Type), string(n.SourceType), string(configRaw), string(secretRaw)).Scan(&id)
	return id, err
}

func (s *Service) getNode(ctx context.Context, id int64) (*NodeView, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, node_type, source_type, config_json, status
FROM clash_proxy_nodes
WHERE id = $1 AND deleted_at IS NULL
`, id)
	var item NodeView
	var configRaw []byte
	if err := row.Scan(&item.ID, &item.Name, &item.NodeType, &item.SourceType, &configRaw, &item.Status); err != nil {
		return nil, err
	}
	if err := decodeJSONMap(configRaw, &item.Config); err != nil {
		return nil, err
	}
	return &item, nil
}
