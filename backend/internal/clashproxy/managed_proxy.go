package clashproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Service) ensureManagedProxy(ctx context.Context, profileID int64, profileName string, mixedPort int) (int64, error) {
	if mixedPort <= 0 || mixedPort > 65535 {
		return 0, errors.New("mihomo mixed port is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var managedProxyID *int64
	if err := tx.QueryRowContext(ctx, `
SELECT managed_proxy_id FROM clash_proxy_profiles WHERE id = $1 AND deleted_at IS NULL FOR UPDATE
`, profileID).Scan(&managedProxyID); err != nil {
		return 0, err
	}
	name := fmt.Sprintf("[Clash] %s", profileName)
	if managedProxyID == nil || *managedProxyID <= 0 {
		var id int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO proxies (name, protocol, host, port, status, fallback_mode, expiry_warn_days)
VALUES ($1, 'http', '127.0.0.1', $2, 'active', 'none', 7)
RETURNING id
`, name, mixedPort).Scan(&id); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE clash_proxy_profiles SET managed_proxy_id = $2, updated_at = NOW() WHERE id = $1
`, profileID, id); err != nil {
			return 0, err
		}
		managedProxyID = &id
	} else {
		result, err := tx.ExecContext(ctx, `
UPDATE proxies
SET name = $2, protocol = 'http', host = '127.0.0.1', port = $3,
    username = NULL, password = NULL, status = 'active', expires_at = NULL,
    fallback_mode = 'none', backup_proxy_id = NULL, deleted_at = NULL, updated_at = NOW()
WHERE id = $1
`, *managedProxyID, name, mixedPort)
		if err != nil {
			return 0, err
		}
		if rows, err := result.RowsAffected(); err == nil && rows == 0 {
			return 0, sql.ErrNoRows
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return *managedProxyID, nil
}

func (s *Service) setManagedProxyStatus(ctx context.Context, profileID int64, status string) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE proxies
SET status = $2, updated_at = NOW()
WHERE id = (SELECT managed_proxy_id FROM clash_proxy_profiles WHERE id = $1)
`, profileID, status)
	return err
}

func (s *Service) getManagedProxyID(ctx context.Context, profileID int64) (*int64, error) {
	var id *int64
	if err := s.db.QueryRowContext(ctx, `
SELECT managed_proxy_id FROM clash_proxy_profiles WHERE id = $1 AND deleted_at IS NULL
`, profileID).Scan(&id); err != nil {
		return nil, err
	}
	return id, nil
}
