package repository

import (
	"context"
	"errors"

	"github.com/TokenFlux/TokenRouter/internal/service"
)

func (r *pluginRepository) PruneSecretGrants(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM sub2api_plugin_secret_grants WHERE expires_at<=NOW()")
	return err
}

func (r *pluginRepository) ListSecretGrants(ctx context.Context, id int64) ([]service.PluginSecretGrant, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id,capability,alias,expires_at,updated_at FROM sub2api_plugin_secret_grants
		WHERE plugin_id=$1 AND expires_at>NOW() ORDER BY capability,alias`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	grants := make([]service.PluginSecretGrant, 0)
	for rows.Next() {
		var grant service.PluginSecretGrant
		if err := rows.Scan(&grant.PluginID, &grant.Capability, &grant.Alias, &grant.ExpiresAt, &grant.UpdatedAt); err != nil {
			return nil, err
		}
		grants = append(grants, grant)
	}
	return grants, rows.Err()
}
func (r *pluginRepository) PutSecretGrant(ctx context.Context, grant service.PluginSecretGrant) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM sub2api_plugin_installations p WHERE id=$1 AND
		(p.manifest->>'schema_version')='2' AND EXISTS (
			SELECT 1 FROM jsonb_array_elements(p.manifest->'capabilities') c
			WHERE c->>'id'=$2 AND c->'permissions' ? 'secrets.broker'
		) FOR UPDATE`, grant.PluginID, grant.Capability).Scan(&id)
	if err != nil {
		return service.ErrPluginStateChanged
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM sub2api_plugin_secret_grants WHERE plugin_id=$1 AND expires_at<=NOW()", grant.PluginID); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sub2api_plugin_secret_grants WHERE plugin_id=$1 AND
		NOT(capability=$2 AND alias=$3)`, grant.PluginID, grant.Capability, grant.Alias).Scan(&count); err != nil {
		return err
	}
	if count >= 32 {
		return errors.New("每个插件最多 32 个有效秘密授权")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sub2api_plugin_secret_grants(plugin_id,capability,alias,value_encrypted,expires_at,updated_at)
		VALUES($1,$2,$3,$4,$5,NOW()) ON CONFLICT(plugin_id,capability,alias) DO UPDATE SET
		value_encrypted=EXCLUDED.value_encrypted,expires_at=EXCLUDED.expires_at,updated_at=NOW()`,
		grant.PluginID, grant.Capability, grant.Alias, grant.EncryptedValue, grant.ExpiresAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (r *pluginRepository) GetSecretGrant(ctx context.Context, id int64, capability, alias string) (*service.PluginSecretGrant, error) {
	grant := &service.PluginSecretGrant{}
	err := r.db.QueryRowContext(ctx, `SELECT plugin_id,capability,alias,value_encrypted,expires_at,updated_at
		FROM sub2api_plugin_secret_grants WHERE plugin_id=$1 AND capability=$2 AND alias=$3 AND expires_at>NOW()`,
		id, capability, alias).Scan(&grant.PluginID, &grant.Capability, &grant.Alias, &grant.EncryptedValue, &grant.ExpiresAt, &grant.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return grant, nil
}
func (r *pluginRepository) DeleteSecretGrant(ctx context.Context, id int64, capability, alias string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM sub2api_plugin_secret_grants WHERE plugin_id=$1 AND capability=$2 AND alias=$3", id, capability, alias)
	return err
}

var _ service.PluginSecretRepository = (*pluginRepository)(nil)
