package clashproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/profile"
	proxyruntime "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/runtime"
	"github.com/TokenFlux/TokenRouter/internal/util/urlvalidator"
)

func (s *Service) ListProfiles(ctx context.Context) ([]ProfileView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, strategy, test_url, interval_seconds, status, config_json, managed_proxy_id
FROM clash_proxy_profiles
WHERE deleted_at IS NULL
ORDER BY id DESC
`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]ProfileView, 0)
	for rows.Next() {
		item, err := scanProfileView(rows)
		if err != nil {
			return nil, err
		}
		item.NodeIDs, err = s.listProfileNodeIDs(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) CreateProfile(ctx context.Context, input CreateProfileInput) (*ProfileView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	normalized, strategy, err := normalizeProfileInput(input)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateProfileNodes(ctx, tx, normalized.NodeIDs); err != nil {
		return nil, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `
INSERT INTO clash_proxy_profiles (name, strategy, test_url, interval_seconds, status)
VALUES ($1, $2, $3, $4, 'active')
RETURNING id
`, normalized.Name, string(strategy), normalized.TestURL, normalized.IntervalSeconds).Scan(&id); err != nil {
		return nil, err
	}
	if err := replaceProfileNodes(ctx, tx, id, normalized.NodeIDs, normalized.Weights); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.getProfile(ctx, id)
}

func (s *Service) UpdateProfile(ctx context.Context, id int64, input UpdateProfileInput) (*ProfileView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("profile id is required")
	}
	normalized, strategy, err := normalizeProfileInput(input)
	if err != nil {
		return nil, err
	}
	running := false
	if instance, runtimeErr := s.runtimeStore.GetRuntime(ctx, id); runtimeErr == nil && instance != nil {
		running = instance.Status == proxyruntime.StatusRunning
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateProfileNodes(ctx, tx, normalized.NodeIDs); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE clash_proxy_profiles
SET name = $2, strategy = $3, test_url = $4, interval_seconds = $5, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
`, id, normalized.Name, string(strategy), normalized.TestURL, normalized.IntervalSeconds)
	if err != nil {
		return nil, err
	}
	if rows, err := result.RowsAffected(); err == nil && rows == 0 {
		return nil, sql.ErrNoRows
	}
	if err := replaceProfileNodes(ctx, tx, id, normalized.NodeIDs, normalized.Weights); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if running {
		if _, err := s.RestartProfile(ctx, id); err != nil {
			return nil, fmt.Errorf("profile saved but runtime restart failed: %w", err)
		}
	}
	return s.getProfile(ctx, id)
}

func (s *Service) StartProfile(ctx context.Context, id int64) (*RuntimeView, error) {
	if err := s.requireRuntime(); err != nil {
		return nil, err
	}
	prof, err := s.getProfileModel(ctx, id)
	if err != nil {
		return nil, err
	}
	instance, err := s.runtimeManager.EnsureRunning(ctx, *prof)
	if err != nil {
		return nil, err
	}
	managedProxyID, err := s.ensureManagedProxy(ctx, prof.ID, prof.Name, instance.MixedPort)
	if err != nil {
		_ = s.runtimeManager.Stop(prof.ID)
		return nil, err
	}
	if err := s.applyProfileBindings(ctx, prof.ID, managedProxyID); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, cleanupErr := s.StopProfile(cleanupCtx, prof.ID); cleanupErr != nil {
			return nil, fmt.Errorf("apply account bindings: %w", errors.Join(err, fmt.Errorf("rollback started profile: %w", cleanupErr)))
		}
		return nil, fmt.Errorf("apply account bindings: %w", err)
	}
	return runtimeView(instance), nil
}

func (s *Service) StopProfile(ctx context.Context, id int64) (*RuntimeView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errors.New("profile id is required")
	}
	var errs []error
	if s.runtimeManager != nil {
		errs = append(errs, s.runtimeManager.Stop(id))
	} else if s.runtimeStore != nil {
		errs = append(errs, s.runtimeStore.MarkRuntimeStopped(ctx, id))
	}
	errs = append(errs, s.restoreProfileBindings(ctx, id))
	errs = append(errs, s.setManagedProxyStatus(ctx, id, "disabled"))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s.GetRuntime(ctx, id)
}

func (s *Service) RestartProfile(ctx context.Context, id int64) (*RuntimeView, error) {
	if _, err := s.StopProfile(ctx, id); err != nil {
		return nil, err
	}
	return s.StartProfile(ctx, id)
}

func (s *Service) GetRuntime(ctx context.Context, id int64) (*RuntimeView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	instance, err := s.runtimeStore.GetRuntime(ctx, id)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return &RuntimeView{ProfileID: id, Status: string(proxyruntime.StatusStopped)}, nil
	}
	return runtimeView(instance), nil
}

func (s *Service) TestProfile(ctx context.Context, id int64) (*ProfileTestView, error) {
	view, err := s.GetRuntime(ctx, id)
	if err != nil {
		return nil, err
	}
	result := &ProfileTestView{ProfileID: id, Status: view.Status, ProxyURL: view.ProxyURL}
	if view.Status != string(proxyruntime.StatusRunning) {
		result.Error = "proxy runtime is not running"
		return result, nil
	}
	instance, err := s.runtimeStore.GetRuntime(ctx, id)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		result.Status = string(proxyruntime.StatusStopped)
		result.Error = "proxy runtime is not running"
		return result, nil
	}
	info, err := s.controllerClient.Version(ctx, *instance, instance.ControllerSecretRef)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.Healthy = true
	result.Version = info.Version
	return result, nil
}

func (s *Service) GetRuntimeStatus(ctx context.Context) (*RuntimeStatusView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT status, COUNT(*) FROM clash_proxy_runtime_instances GROUP BY status
`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	status := &RuntimeStatusView{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		status.Total += count
		switch proxyruntime.Status(name) {
		case proxyruntime.StatusStarting:
			status.Starting += count
		case proxyruntime.StatusRunning:
			status.Running += count
		case proxyruntime.StatusFailed:
			status.Failed += count
		case proxyruntime.StatusStopped:
			status.Stopped += count
		}
	}
	return status, rows.Err()
}

func normalizeProfileInput(input CreateProfileInput) (CreateProfileInput, profile.Strategy, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return input, "", errors.New("profile name is required")
	}
	strategy := profile.Strategy(strings.TrimSpace(input.Strategy))
	if strategy == "" {
		strategy = profile.StrategySelect
	}
	if !profile.IsAllowedStrategy(strategy) {
		return input, "", errors.New("proxy profile strategy is not supported")
	}
	if len(input.NodeIDs) == 0 {
		return input, "", errors.New("at least one proxy node is required")
	}
	if input.IntervalSeconds <= 0 {
		input.IntervalSeconds = 300
	}
	input.TestURL = strings.TrimSpace(input.TestURL)
	if input.TestURL == "" {
		input.TestURL = defaultTestURL
	}
	normalizedURL, err := urlvalidator.ValidateHTTPSURL(input.TestURL, urlvalidator.ValidationOptions{AllowPrivate: false})
	if err != nil {
		return input, "", fmt.Errorf("invalid profile test URL: %w", err)
	}
	input.TestURL = normalizedURL
	return input, strategy, nil
}

func validateProfileNodes(ctx context.Context, tx *sql.Tx, nodeIDs []int64) error {
	seen := make(map[int64]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		if id <= 0 {
			return errors.New("proxy node id is invalid")
		}
		if _, ok := seen[id]; ok {
			return errors.New("proxy profile contains duplicate node ids")
		}
		seen[id] = struct{}{}
		var exists bool
		if err := tx.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM clash_proxy_nodes WHERE id = $1 AND status = 'active' AND deleted_at IS NULL)
`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("proxy node %d does not exist or is disabled", id)
		}
	}
	return nil
}

func replaceProfileNodes(ctx context.Context, tx *sql.Tx, profileID int64, nodeIDs []int64, weights []int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM clash_proxy_profile_nodes WHERE profile_id = $1`, profileID); err != nil {
		return err
	}
	for index, nodeID := range nodeIDs {
		weight := 1
		if index < len(weights) && weights[index] > 0 {
			weight = weights[index]
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO clash_proxy_profile_nodes (profile_id, node_id, sort_order, weight, enabled)
VALUES ($1, $2, $3, $4, TRUE)
`, profileID, nodeID, index, weight); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) getProfile(ctx context.Context, id int64) (*ProfileView, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, strategy, test_url, interval_seconds, status, config_json, managed_proxy_id
FROM clash_proxy_profiles
WHERE id = $1 AND deleted_at IS NULL
`, id)
	item, err := scanProfileView(row)
	if err != nil {
		return nil, err
	}
	item.NodeIDs, err = s.listProfileNodeIDs(ctx, id)
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func scanProfileView(row scanner) (ProfileView, error) {
	var item ProfileView
	var configRaw []byte
	if err := row.Scan(&item.ID, &item.Name, &item.Strategy, &item.TestURL, &item.IntervalSeconds, &item.Status, &configRaw, &item.ManagedProxyID); err != nil {
		return ProfileView{}, err
	}
	if err := decodeJSONMap(configRaw, &item.Config); err != nil {
		return ProfileView{}, err
	}
	return item, nil
}

func (s *Service) listProfileNodeIDs(ctx context.Context, profileID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT node_id FROM clash_proxy_profile_nodes WHERE profile_id = $1 AND enabled = TRUE ORDER BY sort_order, id
`, profileID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Service) getProfileModel(ctx context.Context, id int64) (*profile.Profile, error) {
	if id <= 0 {
		return nil, errors.New("profile id is required")
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, strategy, test_url, interval_seconds, status, config_json
FROM clash_proxy_profiles
WHERE id = $1 AND deleted_at IS NULL
`, id)
	var prof profile.Profile
	var strategy, status string
	var configRaw []byte
	if err := row.Scan(&prof.ID, &prof.Name, &strategy, &prof.TestURL, &prof.IntervalSeconds, &status, &configRaw); err != nil {
		return nil, err
	}
	prof.Strategy = profile.Strategy(strategy)
	prof.Status = profile.Status(status)
	if err := decodeJSONMap(configRaw, &prof.Config); err != nil {
		return nil, err
	}
	return &prof, nil
}

func runtimeView(instance *proxyruntime.Instance) *RuntimeView {
	if instance == nil {
		return nil
	}
	view := &RuntimeView{
		ProfileID:      instance.ProfileID,
		RuntimeType:    instance.RuntimeType,
		PID:            instance.PID,
		MixedPort:      instance.MixedPort,
		ControllerPort: instance.ControllerPort,
		Status:         string(instance.Status),
		LastError:      instance.LastError,
	}
	if proxyURL, err := instance.ProxyURL(); err == nil {
		view.ProxyURL = proxyURL
	}
	return view
}
