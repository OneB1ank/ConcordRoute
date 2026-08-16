package clashproxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	proxynode "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/node"
	proxyruntime "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/runtime"
)

type Service struct {
	db               *sql.DB
	options          Options
	accountUpdater   AccountProxyUpdater
	runtimeManager   *proxyruntime.ProcessManager
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
	svc.runtimeManager = &proxyruntime.ProcessManager{
		BinaryPath:     strings.TrimSpace(options.MihomoBinaryPath),
		RuntimeRoot:    strings.TrimSpace(options.RuntimeRoot),
		Nodes:          proxynode.NewSQLStore(db),
		Instances:      runtimeStore,
		PortAllocator:  proxyruntime.NewPortAllocator(),
		Controller:     controller,
		StartupTimeout: options.StartupTimeout,
	}
	svc.runtimeManager.OnUnexpectedExit = func(ctx context.Context, profileID int64, _ error) error {
		return errors.Join(
			svc.restoreProfileBindings(ctx, profileID),
			svc.setManagedProxyStatus(ctx, profileID, "disabled"),
		)
	}
	return svc
}

func (s *Service) SetControllerHTTPClient(client *http.Client) {
	if s == nil {
		return
	}
	s.controllerClient.HTTPClient = client
	if s.runtimeManager != nil {
		s.runtimeManager.Controller.HTTPClient = client
	}
}

func (s *Service) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(context.Background(), `
SELECT id FROM clash_proxy_profiles WHERE deleted_at IS NULL ORDER BY id
`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var errs []error
	for _, id := range ids {
		_, stopErr := s.StopProfile(context.Background(), id)
		errs = append(errs, stopErr)
	}
	return errors.Join(errs...)
}

// ReconcileStartup 清理异常退出遗留的运行态，并恢复账号原来的代理。
func (s *Service) ReconcileStartup(ctx context.Context) error {
	if err := s.requireConfigured(); err != nil {
		return err
	}
	if err := s.runtimeStore.MarkAllRunningStopped(ctx); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM clash_proxy_profiles WHERE managed_proxy_id IS NOT NULL AND deleted_at IS NULL ORDER BY id
`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var errs []error
	for _, id := range ids {
		errs = append(errs, s.restoreProfileBindings(ctx, id))
		errs = append(errs, s.setManagedProxyStatus(ctx, id, "disabled"))
	}
	return errors.Join(errs...)
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
