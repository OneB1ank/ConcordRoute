package clashproxy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const (
	bulkBindingOpenAIPlatform = "openai"
	bulkBindingOAuthType      = "oauth"
)

func (s *Service) ListBindings(ctx context.Context) ([]BindingView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT b.id, b.account_id, a.name, a.platform, b.profile_id, p.name, b.previous_proxy_id, b.enabled
FROM clash_proxy_account_bindings b
JOIN accounts a ON a.id = b.account_id AND a.deleted_at IS NULL
JOIN clash_proxy_profiles p ON p.id = b.profile_id AND p.deleted_at IS NULL
ORDER BY b.id DESC
`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]BindingView, 0)
	for rows.Next() {
		var item BindingView
		if err := rows.Scan(
			&item.ID,
			&item.AccountID,
			&item.AccountName,
			&item.AccountPlatform,
			&item.ProfileID,
			&item.ProfileName,
			&item.PreviousProxyID,
			&item.Enabled,
		); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Service) CreateBinding(ctx context.Context, input CreateBindingInput) (*BindingView, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	if input.AccountID <= 0 || input.ProfileID <= 0 {
		return nil, errors.New("account_id and profile_id are required")
	}
	if s.accountUpdater == nil {
		return nil, errors.New("account proxy updater is not configured")
	}
	runtime, err := s.runtimeStore.GetRuntime(ctx, input.ProfileID)
	if err != nil {
		return nil, err
	}
	if runtime == nil || runtime.Status != "running" {
		return nil, errors.New("start the proxy profile before binding an account")
	}
	managedProxyID, err := s.getManagedProxyID(ctx, input.ProfileID)
	if err != nil {
		return nil, err
	}
	if managedProxyID == nil || *managedProxyID <= 0 {
		return nil, errors.New("proxy profile has no managed local proxy")
	}
	return s.createBindingWithManagedProxy(ctx, input, *managedProxyID)
}

func (s *Service) createBindingWithManagedProxy(ctx context.Context, input CreateBindingInput, managedProxyID int64) (*BindingView, error) {
	currentProxyID, err := s.getAccountProxyID(ctx, input.AccountID)
	if err != nil {
		return nil, err
	}
	oldBinding, err := s.getBindingByAccount(ctx, input.AccountID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	previousProxyID := currentProxyID
	if oldBinding != nil {
		previousProxyID = oldBinding.PreviousProxyID
	} else if currentProxyID != nil {
		managed, err := s.isManagedProxy(ctx, *currentProxyID)
		if err != nil {
			return nil, err
		}
		if managed {
			previousProxyID = nil
		}
	}

	var bindingID int64
	if err := s.db.QueryRowContext(ctx, `
INSERT INTO clash_proxy_account_bindings (account_id, profile_id, previous_proxy_id, enabled)
VALUES ($1, $2, $3, TRUE)
ON CONFLICT (account_id) DO UPDATE SET
    profile_id = EXCLUDED.profile_id,
    previous_proxy_id = EXCLUDED.previous_proxy_id,
    enabled = TRUE,
    updated_at = NOW()
RETURNING id
`, input.AccountID, input.ProfileID, previousProxyID).Scan(&bindingID); err != nil {
		return nil, err
	}
	if err := s.accountUpdater.SetAccountProxy(ctx, input.AccountID, &managedProxyID); err != nil {
		_ = s.rollbackBinding(ctx, input.AccountID, oldBinding)
		return nil, err
	}
	return s.getBinding(ctx, bindingID)
}

// @project-doc docs/operations/upstream_transport_security.md#clash_account_binding
// BindUnboundOpenAIOAuthAccounts 把尚未配置出口的 OpenAI OAuth 主账号批量绑定到运行中策略。
// 已有账号代理保持不变；影子账号由既有账号更新链路跟随主账号同步。
func (s *Service) BindUnboundOpenAIOAuthAccounts(ctx context.Context, profileID int64) (*BulkBindOpenAIOAuthResult, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	if profileID <= 0 {
		return nil, errors.New("profile id is required")
	}
	if s.accountUpdater == nil {
		return nil, errors.New("account proxy updater is not configured")
	}
	runtime, err := s.runtimeStore.GetRuntime(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if runtime == nil || runtime.Status != "running" {
		return nil, errors.New("start the proxy profile before binding accounts")
	}
	managedProxyID, err := s.getManagedProxyID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	if managedProxyID == nil || *managedProxyID <= 0 {
		return nil, errors.New("proxy profile has no managed local proxy")
	}

	accountIDs, err := s.listUnboundOpenAIOAuthAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	result := &BulkBindOpenAIOAuthResult{
		ProfileID: profileID,
		Eligible:  len(accountIDs),
		Failures:  make([]BulkBindOpenAIOAuthFailure, 0),
	}
	for _, accountID := range accountIDs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, err := s.createBindingWithManagedProxy(ctx, CreateBindingInput{AccountID: accountID, ProfileID: profileID}, *managedProxyID); err != nil {
			result.Failed++
			result.Failures = append(result.Failures, BulkBindOpenAIOAuthFailure{AccountID: accountID, Reason: err.Error()})
			continue
		}
		result.Bound++
	}
	return result, nil
}

func (s *Service) listUnboundOpenAIOAuthAccountIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id
FROM accounts
WHERE deleted_at IS NULL
  AND parent_account_id IS NULL
  AND LOWER(platform) = $1
  AND LOWER(type) = $2
  AND proxy_id IS NULL
ORDER BY id
`, bulkBindingOpenAIPlatform, bulkBindingOAuthType)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	accountIDs := make([]int64, 0)
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	return accountIDs, rows.Err()
}

func (s *Service) DeleteBinding(ctx context.Context, id int64) error {
	if err := s.requireConfigured(); err != nil {
		return err
	}
	if id <= 0 {
		return errors.New("binding id is required")
	}
	binding, err := s.getBinding(ctx, id)
	if err != nil {
		return err
	}
	previousProxyID, err := s.validPreviousProxyID(ctx, binding.PreviousProxyID)
	if err != nil {
		return err
	}
	if s.accountUpdater == nil {
		return errors.New("account proxy updater is not configured")
	}
	if err := s.accountUpdater.SetAccountProxy(ctx, binding.AccountID, previousProxyID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM clash_proxy_account_bindings WHERE id = $1`, id)
	return err
}

func (s *Service) applyProfileBindings(ctx context.Context, profileID, managedProxyID int64) error {
	if s.accountUpdater == nil {
		return errors.New("account proxy updater is not configured")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT account_id FROM clash_proxy_account_bindings WHERE profile_id = $1 AND enabled = TRUE ORDER BY id
`, profileID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var errs []error
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return err
		}
		proxyID := managedProxyID
		if err := s.accountUpdater.SetAccountProxy(ctx, accountID, &proxyID); err != nil {
			errs = append(errs, fmt.Errorf("account %d: %w", accountID, err))
		}
	}
	if err := rows.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Service) getBinding(ctx context.Context, id int64) (*BindingView, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT b.id, b.account_id, a.name, a.platform, b.profile_id, p.name, b.previous_proxy_id, b.enabled
FROM clash_proxy_account_bindings b
JOIN accounts a ON a.id = b.account_id AND a.deleted_at IS NULL
JOIN clash_proxy_profiles p ON p.id = b.profile_id AND p.deleted_at IS NULL
WHERE b.id = $1
`, id)
	var item BindingView
	if err := row.Scan(
		&item.ID,
		&item.AccountID,
		&item.AccountName,
		&item.AccountPlatform,
		&item.ProfileID,
		&item.ProfileName,
		&item.PreviousProxyID,
		&item.Enabled,
	); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Service) getBindingByAccount(ctx context.Context, accountID int64) (*BindingView, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT b.id, b.account_id, a.name, a.platform, b.profile_id, p.name, b.previous_proxy_id, b.enabled
FROM clash_proxy_account_bindings b
JOIN accounts a ON a.id = b.account_id AND a.deleted_at IS NULL
JOIN clash_proxy_profiles p ON p.id = b.profile_id AND p.deleted_at IS NULL
WHERE b.account_id = $1
`, accountID)
	var item BindingView
	if err := row.Scan(
		&item.ID,
		&item.AccountID,
		&item.AccountName,
		&item.AccountPlatform,
		&item.ProfileID,
		&item.ProfileName,
		&item.PreviousProxyID,
		&item.Enabled,
	); err != nil {
		return nil, err
	}
	return &item, nil
}

func (s *Service) getAccountProxyID(ctx context.Context, accountID int64) (*int64, error) {
	var proxyID *int64
	if err := s.db.QueryRowContext(ctx, `
SELECT proxy_id FROM accounts WHERE id = $1 AND deleted_at IS NULL
`, accountID).Scan(&proxyID); err != nil {
		return nil, err
	}
	return proxyID, nil
}

func (s *Service) isManagedProxy(ctx context.Context, proxyID int64) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM clash_proxy_profiles WHERE managed_proxy_id = $1 AND deleted_at IS NULL)
`, proxyID).Scan(&exists)
	return exists, err
}

func (s *Service) validPreviousProxyID(ctx context.Context, proxyID *int64) (*int64, error) {
	if proxyID == nil || *proxyID <= 0 {
		return nil, nil
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `
SELECT EXISTS(SELECT 1 FROM proxies WHERE id = $1 AND deleted_at IS NULL)
`, *proxyID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return proxyID, nil
}

func (s *Service) rollbackBinding(ctx context.Context, accountID int64, old *BindingView) error {
	if old == nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM clash_proxy_account_bindings WHERE account_id = $1`, accountID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE clash_proxy_account_bindings
SET profile_id = $2, previous_proxy_id = $3, enabled = $4, updated_at = NOW()
WHERE account_id = $1
`, accountID, old.ProfileID, old.PreviousProxyID, old.Enabled)
	return err
}
