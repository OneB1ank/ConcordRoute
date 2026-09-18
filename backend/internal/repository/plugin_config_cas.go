package repository

import (
	"context"
	"github.com/TokenFlux/TokenRouter/internal/service"
)

// Compare the encrypted snapshot as well as the binary. Concurrent managers
// cannot silently overwrite a protection snapshot produced by another instance.
func (r *pluginRepository) UpdateConfigCAS(ctx context.Context, id int64, encrypted, expectedBinary, expectedConfig string) error {
	result, err := r.db.ExecContext(ctx, `UPDATE sub2api_plugin_installations
 SET config_encrypted=$2, updated_at=NOW()
 WHERE id=$1 AND binary_sha256=$3 AND COALESCE(config_encrypted,'')=$4`, id, encrypted, expectedBinary, expectedConfig)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return service.ErrPluginStateChanged
	}
	return nil
}
