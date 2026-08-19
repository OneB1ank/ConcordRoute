package clashproxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	proxynode "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/node"
	proxyprofile "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/profile"
	proxyruntime "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/runtime"
)

type profileRuntimeManager interface {
	EnsureRunning(context.Context, proxyprofile.Profile) (*proxyruntime.Instance, error)
	Stop(int64) error
	SetControllerHTTPClient(*http.Client)
}

type Service struct {
	db               *sql.DB
	options          Options
	accountUpdater   AccountProxyUpdater
	runtimeManager   profileRuntimeManager
	runtimeStore     *proxyruntime.SQLStore
	controllerClient proxyruntime.ControllerClient
}

func NewService(db *sql.DB, options Options, updater AccountProxyUpdater) *Service {
	runtimeStore := proxyruntime.NewSQLStore(db)
	controller := proxyruntime.ControllerClient{}
	svc := &Service{
		db:               db,
		options:          options,
		accountUpdater:   updater,
		runtimeStore:     runtimeStore,
		controllerClient: controller,
	}
	runtimeManager := &proxyruntime.ProcessManager{
		BinaryPath:     strings.TrimSpace(options.MihomoBinaryPath),
		RuntimeRoot:    strings.TrimSpace(options.RuntimeRoot),
		Nodes:          proxynode.NewSQLStore(db),
		Instances:      runtimeStore,
		PortAllocator:  proxyruntime.NewPortAllocator(),
		Controller:     controller,
		StartupTimeout: options.StartupTimeout,
	}
	runtimeManager.OnUnexpectedExit = func(ctx context.Context, profileID int64, _ error) error {
		// 绑定表示管理员明确选择了该代理。运行时异常退出时只把托管代理标记为
		// 不可用，保留账号的 proxy_id，避免请求静默回退为直连或旧代理。
		return svc.setManagedProxyStatus(ctx, profileID, "disabled")
	}
	svc.runtimeManager = runtimeManager
	return svc
}

func (s *Service) SetControllerHTTPClient(client *http.Client) {
	if s == nil {
		return
	}
	s.controllerClient.HTTPClient = client
	if s.runtimeManager != nil {
		s.runtimeManager.SetControllerHTTPClient(client)
	}
}

func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	ids, err := s.listProfileIDs(context.Background(), `
SELECT id FROM clash_proxy_profiles WHERE deleted_at IS NULL ORDER BY id
`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		// 应用退出只停止子进程，自启动配置仍由策略自身保存。
		_, stopErr := s.stopProfile(context.Background(), id)
		errs = append(errs, stopErr)
	}
	return errors.Join(errs...)
}

// ReconcileStartup 清理异常退出遗留状态，并恢复用户此前启动的策略和账号绑定。
func (s *Service) ReconcileStartup(ctx context.Context) error {
	if err := s.requireConfigured(); err != nil {
		return err
	}
	if err := s.runtimeStore.MarkAllRunningStopped(ctx); err != nil {
		return err
	}
	cleanupIDs, err := s.listProfileIDs(ctx, `
SELECT id FROM clash_proxy_profiles WHERE managed_proxy_id IS NOT NULL AND deleted_at IS NULL ORDER BY id
`)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range cleanupIDs {
		errs = append(errs, s.setManagedProxyStatus(ctx, id, "disabled"))
		managedProxyID, proxyErr := s.getManagedProxyID(ctx, id)
		if proxyErr != nil {
			errs = append(errs, fmt.Errorf("load managed proxy for clash profile %d: %w", id, proxyErr))
			continue
		}
		if managedProxyID != nil && *managedProxyID > 0 {
			// enabled binding 是期望路由的唯一来源：绑定存在就始终指向本地
			// 托管代理。若 mihomo 尚未启动，请求应明确失败，而不是泄漏到直连。
			errs = append(errs, s.applyProfileBindings(ctx, id, *managedProxyID))
		}
	}

	autoStartIDs, err := s.listProfileIDs(ctx, `
SELECT id
FROM clash_proxy_profiles
WHERE auto_start = TRUE AND status = 'active' AND deleted_at IS NULL
ORDER BY id
`)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	for _, id := range autoStartIDs {
		if _, err := s.startProfile(ctx, id); err != nil {
			// 单个策略失败时保留 auto_start，后续重启仍会继续尝试恢复。
			errs = append(errs, fmt.Errorf("auto-start clash proxy profile %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) listProfileIDs(ctx context.Context, query string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, query)
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

func (s *Service) requireConfigured() error {
	if s == nil || s.db == nil {
		return errors.New("clash proxy service is not configured")
	}
	return nil
}

func (s *Service) requireRuntime() error {
	if err := s.requireConfigured(); err != nil {
		return err
	}
	if !s.options.Enabled {
		return errors.New("clash proxy runtime is disabled")
	}
	if s.runtimeManager == nil {
		return errors.New("clash proxy runtime manager is not configured")
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
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
