package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/TokenFlux/TokenRouter/internal/service"
)

func (r *pluginRepository) ListVersions(ctx context.Context, id int64) ([]service.PluginVersion, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,plugin_id,version,binary_sha256,saved_at,expires_at,manifest
		FROM sub2api_plugin_versions WHERE plugin_id=$1 AND expires_at>NOW() ORDER BY saved_at DESC,id DESC LIMIT 5`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	versions := make([]service.PluginVersion, 0)
	for rows.Next() {
		var v service.PluginVersion
		var manifest []byte
		if err := rows.Scan(&v.ID, &v.PluginID, &v.Version, &v.BinarySHA256, &v.SavedAt, &v.ExpiresAt, &manifest); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(manifest, &v.Manifest); err != nil {
			return nil, err
		}
		versions = append(versions, v)
	}
	return versions, rows.Err()
}
func (r *pluginRepository) GetVersion(ctx context.Context, id, versionID int64) (*service.PluginInstallation, error) {
	var manifest []byte
	p := &service.PluginInstallation{ID: id}
	err := r.db.QueryRowContext(ctx, `SELECT plugin_key,name,version,description,author,manifest,artifact_data,binary_sha256,signature_status,config_encrypted
		FROM sub2api_plugin_versions WHERE plugin_id=$1 AND id=$2 AND expires_at>NOW()`, id, versionID).
		Scan(&p.PluginKey, &p.Name, &p.Version, &p.Description, &p.Author, &manifest, &p.ArtifactData, &p.BinarySHA256, &p.SignatureStatus, &p.ConfigEncrypted)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(manifest, &p.Manifest); err != nil {
		return nil, err
	}
	return p, nil
}
func (r *pluginRepository) SwapVersion(ctx context.Context, expected, replacement *service.PluginInstallation) (*service.PluginInstallation, error) {
	return r.swapVersionWithPublisher(ctx, expected, replacement, nil)
}

func (r *pluginRepository) SwapVersionWithPublisher(ctx context.Context, expected, replacement *service.PluginInstallation, publisher *service.PluginPublisher) (*service.PluginInstallation, error) {
	return r.swapVersionWithPublisher(ctx, expected, replacement, publisher)
}

func (r *pluginRepository) swapVersionWithPublisher(ctx context.Context, expected, replacement *service.PluginInstallation, publisher *service.PluginPublisher) (*service.PluginInstallation, error) {
	if expected.ID != replacement.ID || expected.PluginKey != replacement.PluginKey {
		return nil, errors.New("插件版本切换身份无效")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if publisher != nil {
		if err := trustPluginPublisher(ctx, tx, publisher, replacement.InstalledBy); err != nil {
			return nil, err
		}
	}
	current, err := scanPlugin(tx.QueryRowContext(ctx, pluginSelectSQL+" WHERE id=$1 FOR UPDATE", expected.ID))
	if err != nil {
		return nil, err
	}
	if current.State == "starting" || current.State != expected.State || current.BinarySHA256 != expected.BinarySHA256 ||
		current.ConfigEncrypted != expected.ConfigEncrypted || !current.UpdatedAt.Equal(expected.UpdatedAt) {
		return nil, service.ErrPluginStateChanged
	}
	archived, err := tx.ExecContext(ctx, `INSERT INTO sub2api_plugin_versions
		(plugin_id,plugin_key,name,version,description,author,manifest,artifact_data,binary_sha256,signature_status,config_encrypted)
		SELECT id,plugin_key,name,version,description,author,manifest,artifact_data,binary_sha256,signature_status,config_encrypted
		FROM sub2api_plugin_installations WHERE id=$1 AND artifact_data IS NOT NULL AND octet_length(artifact_data)>0`, expected.ID)
	if err != nil {
		return nil, err
	}
	if count, err := archived.RowsAffected(); err != nil || count != 1 {
		return nil, errors.New("旧版本插件包缺失，无法保留回滚快照")
	}
	manifest, err := json.Marshal(replacement.Manifest)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sub2api_plugin_installations SET
		name=$2,version=$3,description=$4,author=$5,manifest=$6::jsonb,artifact_data=$7,
		artifact_path=$8,install_path=$9,binary_path=$10,binary_sha256=$11,signature_status=$12,
		config_encrypted=$13,state=$14,last_error='',updated_at=NOW()
		WHERE id=$1`, expected.ID, replacement.Name, replacement.Version, replacement.Description, replacement.Author, manifest, replacement.ArtifactData,
		replacement.ArtifactPath, replacement.InstallPath, replacement.BinaryPath, replacement.BinarySHA256, replacement.SignatureStatus, replacement.ConfigEncrypted, replacement.State)
	if err != nil {
		return nil, err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return nil, service.ErrPluginStateChanged
	}
	// Keep the newest five snapshots for up to 24 hours. Package deletion removes
	// its own history through the foreign key, without touching other plugins.
	if _, err = tx.ExecContext(ctx, `DELETE FROM sub2api_plugin_versions WHERE plugin_id=$1 AND
		(expires_at<=NOW() OR id NOT IN (SELECT id FROM sub2api_plugin_versions WHERE plugin_id=$1 ORDER BY saved_at DESC,id DESC LIMIT 5))`, expected.ID); err != nil {
		return nil, err
	}
	updated, err := scanPlugin(tx.QueryRowContext(ctx, pluginSelectSQL+" WHERE id=$1", expected.ID))
	if err != nil {
		return nil, err
	}
	updated.Bindings = append([]service.PluginBinding(nil), expected.Bindings...)
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

var _ service.PluginVersionRepository = (*pluginRepository)(nil)
