package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type SQLStore struct {
	DB *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

func (s *SQLStore) ListProfileNodes(ctx context.Context, profileID int64) ([]Node, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("proxy node sql store is not configured")
	}
	rows, err := s.DB.QueryContext(ctx, `
SELECT n.id, n.name, n.node_type, n.source_type, n.config_json, n.secret_json
FROM clash_proxy_profile_nodes pn
JOIN clash_proxy_nodes n ON n.id = pn.node_id
WHERE pn.profile_id = $1 AND pn.enabled = TRUE AND n.status = 'active' AND n.deleted_at IS NULL
ORDER BY pn.sort_order ASC, pn.id ASC
`, profileID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var nodes []Node
	for rows.Next() {
		var n Node
		var nodeType, sourceType string
		var configRaw, secretRaw []byte
		if err := rows.Scan(&n.ID, &n.Name, &nodeType, &sourceType, &configRaw, &secretRaw); err != nil {
			return nil, err
		}
		n.Type = Type(nodeType)
		n.SourceType = SourceType(sourceType)
		if err := decodeJSONMap(configRaw, &n.Config); err != nil {
			return nil, fmt.Errorf("decode proxy node config: %w", err)
		}
		if err := decodeJSONMap(secretRaw, &n.Secret); err != nil {
			return nil, fmt.Errorf("decode proxy node secret: %w", err)
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func decodeJSONMap(raw []byte, target *map[string]any) error {
	if len(raw) == 0 {
		*target = map[string]any{}
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	if *target == nil {
		*target = map[string]any{}
	}
	return nil
}
