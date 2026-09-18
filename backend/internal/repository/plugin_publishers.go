package repository

import (
	"context"
	"database/sql"
	"errors"

	"github.com/TokenFlux/TokenRouter/internal/service"
)

func (r *pluginRepository) GetTrustedPublisher(ctx context.Context, keyID string) (*service.PluginPublisher, error) {
	p := &service.PluginPublisher{}
	err := r.db.QueryRowContext(ctx, `SELECT key_id,public_key,fingerprint FROM sub2api_plugin_publishers WHERE key_id=$1`, keyID).Scan(&p.KeyID, &p.PublicKey, &p.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Immutable key IDs prevent a concurrent upload or an embedded self-signed key
// from replacing an administrator's existing pin. A failed installation rolls
// this write back together with the package and bindings.
func trustPluginPublisher(ctx context.Context, tx *sql.Tx, p *service.PluginPublisher, trustedBy *int64) error {
	var public string
	err := tx.QueryRowContext(ctx, `INSERT INTO sub2api_plugin_publishers(key_id,public_key,fingerprint,trusted_by)
		VALUES($1,$2,$3,$4) ON CONFLICT(key_id) DO UPDATE SET key_id=EXCLUDED.key_id
		WHERE sub2api_plugin_publishers.public_key=EXCLUDED.public_key RETURNING public_key`, p.KeyID, p.PublicKey, p.Fingerprint, trustedBy).Scan(&public)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrPluginPublisherKeyChanged
	}
	return err
}

var _ service.PluginPublisherRepository = (*pluginRepository)(nil)
var _ service.PluginPublisherVersionRepository = (*pluginRepository)(nil)
