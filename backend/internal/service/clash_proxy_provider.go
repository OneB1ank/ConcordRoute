package service

import (
	"context"
	"database/sql"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/clashproxy"
	"github.com/TokenFlux/TokenRouter/internal/config"
)

type clashProxyAccountUpdater struct {
	admin AdminService
}

func (u *clashProxyAccountUpdater) SetAccountProxy(ctx context.Context, accountID int64, proxyID *int64) error {
	value := int64(0)
	if proxyID != nil {
		value = *proxyID
	}
	_, err := u.admin.UpdateAccount(ctx, accountID, &UpdateAccountInput{ProxyID: &value})
	return err
}

// ProvideClashProxyService 将 mihomo 运行时接入现有账号代理更新链路。
func ProvideClashProxyService(cfg *config.Config, db *sql.DB, admin AdminService) (*clashproxy.Service, error) {
	svc := clashproxy.NewService(db, clashproxy.Options{
		Enabled:                   cfg.ClashProxy.Enabled,
		MihomoBinaryPath:          cfg.ClashProxy.MihomoBinaryPath,
		RuntimeRoot:               cfg.ClashProxy.RuntimeDir,
		StartupTimeout:            time.Duration(cfg.ClashProxy.StartupTimeoutSeconds) * time.Second,
		SubscriptionMaxBytes:      cfg.ClashProxy.SubscriptionMaxBytes,
		AllowInsecureSubscription: cfg.ClashProxy.AllowInsecureSubscription,
		AllowPrivateSubscription:  cfg.ClashProxy.AllowPrivateSubscription,
	}, &clashProxyAccountUpdater{admin: admin})
	if err := svc.ReconcileStartup(context.Background()); err != nil {
		return nil, err
	}
	return svc, nil
}
