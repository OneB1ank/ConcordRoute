package repository

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/service"
)

func (r *pluginRepository) UpdateRouting(ctx context.Context, expected *service.PluginInstallation, bindings []service.PluginBinding) (*service.PluginInstallation, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_plugin_installations SET updated_at=GREATEST(clock_timestamp(),updated_at+INTERVAL '1 microsecond')
		WHERE id=$1 AND binary_sha256=$2 AND updated_at=$3 AND state=$4 AND state<>'starting'`,
		expected.ID, expected.BinarySHA256, expected.UpdatedAt, expected.State)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return nil, service.ErrPluginStateChanged
	}
	if err := replacePluginBindings(ctx, tx, expected.ID, bindings); err != nil {
		return nil, err
	}
	saved, err := scanPlugin(tx.QueryRowContext(ctx, pluginSelectSQL+" WHERE id=$1", expected.ID))
	if err != nil {
		return nil, err
	}
	saved.Bindings, err = listPluginBindings(ctx, tx, expected.ID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return saved, nil
}

var _ service.PluginRoutingRepository = (*pluginRepository)(nil)
