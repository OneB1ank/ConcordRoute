package runtime

import (
	"context"
	"database/sql"
	"errors"
)

type SQLStore struct {
	DB *sql.DB
}

func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

func (s *SQLStore) GetRunningRuntime(ctx context.Context, profileID int64) (*Instance, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("proxy runtime sql store is not configured")
	}
	row := s.DB.QueryRowContext(ctx, `
SELECT id, profile_id, runtime_type, COALESCE(pid, 0), mixed_port, controller_port, controller_secret_ref, config_path, work_dir, status, COALESCE(last_error, '')
FROM clash_proxy_runtime_instances
WHERE profile_id = $1 AND status = 'running'
LIMIT 1
`, profileID)
	return scanRuntime(row)
}

func (s *SQLStore) GetRuntime(ctx context.Context, profileID int64) (*Instance, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("proxy runtime sql store is not configured")
	}
	row := s.DB.QueryRowContext(ctx, `
SELECT id, profile_id, runtime_type, COALESCE(pid, 0), mixed_port, controller_port, controller_secret_ref, config_path, work_dir, status, COALESCE(last_error, '')
FROM clash_proxy_runtime_instances
WHERE profile_id = $1
LIMIT 1
`, profileID)
	return scanRuntime(row)
}

func (s *SQLStore) SaveRuntimeStarting(ctx context.Context, instance Instance) error {
	if s == nil || s.DB == nil {
		return errors.New("proxy runtime sql store is not configured")
	}
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO clash_proxy_runtime_instances (
    profile_id, runtime_type, pid, mixed_port, controller_port, controller_secret_ref, config_path, work_dir, status, last_error, updated_at
) VALUES ($1, $2, NULL, $3, $4, $5, $6, $7, 'starting', NULL, NOW())
ON CONFLICT (profile_id) DO UPDATE SET
    runtime_type = EXCLUDED.runtime_type,
    pid = NULL,
    mixed_port = EXCLUDED.mixed_port,
    controller_port = EXCLUDED.controller_port,
    controller_secret_ref = EXCLUDED.controller_secret_ref,
    config_path = EXCLUDED.config_path,
    work_dir = EXCLUDED.work_dir,
    status = 'starting',
    last_error = NULL,
    updated_at = NOW()
`, instance.ProfileID, instance.RuntimeType, instance.MixedPort, instance.ControllerPort, instance.ControllerSecretRef, instance.ConfigPath, instance.WorkDir)
	return err
}

func (s *SQLStore) MarkRuntimeRunning(ctx context.Context, profileID int64, pid int) error {
	if s == nil || s.DB == nil {
		return errors.New("proxy runtime sql store is not configured")
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE clash_proxy_runtime_instances
SET pid = $2, status = 'running', started_at = NOW(), last_error = NULL, updated_at = NOW()
WHERE profile_id = $1
`, profileID, pid)
	return err
}

func (s *SQLStore) MarkRuntimeFailed(ctx context.Context, profileID int64, errMsg string) error {
	if s == nil || s.DB == nil {
		return errors.New("proxy runtime sql store is not configured")
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE clash_proxy_runtime_instances
SET status = 'failed', last_error = $2, updated_at = NOW()
WHERE profile_id = $1
`, profileID, errMsg)
	return err
}

func (s *SQLStore) MarkRuntimeStopped(ctx context.Context, profileID int64) error {
	if s == nil || s.DB == nil {
		return errors.New("proxy runtime sql store is not configured")
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE clash_proxy_runtime_instances
SET pid = NULL, status = 'stopped', updated_at = NOW()
WHERE profile_id = $1
`, profileID)
	return err
}

func (s *SQLStore) MarkAllRunningStopped(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("proxy runtime sql store is not configured")
	}
	_, err := s.DB.ExecContext(ctx, `
UPDATE clash_proxy_runtime_instances
SET pid = NULL, status = 'stopped', updated_at = NOW()
WHERE status IN ('starting', 'running')
`)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRuntime(row scanner) (*Instance, error) {
	var instance Instance
	var status string
	if err := row.Scan(
		&instance.ID,
		&instance.ProfileID,
		&instance.RuntimeType,
		&instance.PID,
		&instance.MixedPort,
		&instance.ControllerPort,
		&instance.ControllerSecretRef,
		&instance.ConfigPath,
		&instance.WorkDir,
		&status,
		&instance.LastError,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	instance.Status = Status(status)
	return &instance, nil
}
