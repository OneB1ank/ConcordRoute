package service

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/pkg/logger"
)

// 验证新安装、部署关闭和旧数据库的显式关闭三种状态分别保留原意。
func TestOpsCleanupDefaultsRespectDeploymentAndExplicitSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deployment bool
		raw        string
		want       bool
	}{
		{"enabled deployment", true, "", true},
		{"disabled deployment", false, "", false},
		{"legacy partial", true, `{"display_alert_events":false}`, true},
		{"explicit disabled", true, `{"data_retention":{"cleanup_enabled":false}}`, false},
		{"explicit enabled", false, `{"data_retention":{"cleanup_enabled":true}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRuntimeSettingRepoStub()
			if tc.raw != "" {
				repo.values[SettingKeyOpsAdvancedSettings] = tc.raw
			}
			svc := &OpsService{settingRepo: repo, cfg: &config.Config{Ops: config.OpsConfig{Cleanup: config.OpsCleanupConfig{Enabled: tc.deployment}}}}
			svc.initRuntimeSettings(context.Background())
			got, err := svc.GetOpsAdvancedSettings(context.Background())
			if err != nil || got.DataRetention.CleanupEnabled != tc.want {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
	svc := &OpsService{}
	got, _ := svc.GetOpsAdvancedSettings(context.Background())
	if !got.DataRetention.CleanupEnabled {
		t.Fatal("default cleanup is disabled")
	}
}

// 重置失败应恢复日志器及落库策略，重置成功后两者一同回归默认并刷新清理任务。
func TestResetRuntimeLogConfigRestoresAccessLogPolicy(t *testing.T) {
	t.Cleanup(func() { _ = applyOpsRuntimeLogConfig(defaultOpsRuntimeLogConfig(nil)) })
	for _, fail := range []bool{true, false} {
		t.Run(map[bool]string{true: "delete failure", false: "success"}[fail], func(t *testing.T) {
			repo := newRuntimeSettingRepoStub()
			old := defaultOpsRuntimeLogConfig(nil)
			old.Level = "debug"
			old.PersistAccessLogs = true
			raw, _ := json.Marshal(old)
			repo.values[SettingKeyOpsRuntimeLogConfig] = string(raw)
			if fail {
				repo.deleteFn = func(string) error { return errors.New("delete failed") }
			}
			sink := &OpsSystemLogSink{}
			reload := &runtimeCleanupReloader{}
			svc := &OpsService{settingRepo: repo, systemLogSink: sink, cleanupReloader: reload}
			svc.applyRuntimeLogConfigOnStartup(context.Background())
			got, err := svc.ResetRuntimeLogConfig(context.Background(), 1)
			if fail {
				if err == nil || !sink.persistAccessLogs.Load() || logger.CurrentLevel() != "debug" || reload.calls != 0 {
					t.Fatalf("rollback failed err=%v policy=%v level=%s reload=%d", err, sink.persistAccessLogs.Load(), logger.CurrentLevel(), reload.calls)
				}
			} else {
				if err != nil || got.PersistAccessLogs || sink.persistAccessLogs.Load() || logger.CurrentLevel() != "info" || reload.calls != 1 {
					t.Fatalf("reset failed got=%+v err=%v", got, err)
				}
			}
		})
	}
}

// 直接验证清理 SQL 使用系统日志的七天截止日期，不受错误日志跳过清理的设置影响。
type opsRetentionCutoff struct{ earliest, timeLimit time.Time }

func (m opsRetentionCutoff) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	return ok && !got.Before(m.earliest) && !got.After(m.timeLimit)
}
func TestOpsCleanupSystemLogAndAuditShareRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	cutoff := opsRetentionCutoff{time.Now().UTC().AddDate(0, 0, -7).Add(-time.Second), time.Now().UTC().AddDate(0, 0, -7).Add(time.Minute)}
	for _, table := range []string{"ops_system_logs", "ops_system_log_cleanup_audits"} {
		mock.ExpectExec(`(?s)WITH batch AS MATERIALIZED.*DELETE FROM `+table+` AS target`).WithArgs(cutoff, 1000).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	svc := &OpsCleanupService{db: db, cfg: &config.Config{}, effective: config.OpsCleanupConfig{ErrorLogRetentionDays: -1, SystemLogRetentionDays: 7, MinuteMetricsRetentionDays: -1, HourlyMetricsRetentionDays: -1, BatchSize: 1000}}
	counts, err := svc.runCleanupOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.systemLogs != 1 || counts.logAudits != 1 {
		t.Fatalf("counts=%+v", counts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 构造已读到旧值但尚未应用的刷新，验证它不会覆盖随后完成的管理保存。
type opsBlockedRefreshRepo struct {
	*runtimeSettingRepoStub
	read   chan struct{}
	resume chan struct{}
}

func (r *opsBlockedRefreshRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	values, err := r.runtimeSettingRepoStub.GetMultiple(ctx, keys)
	close(r.read)
	select {
	case <-r.resume:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return values, err
}
func TestRuntimeLogSaveSurvivesInFlightStaleRefresh(t *testing.T) {
	t.Cleanup(func() { _ = applyOpsRuntimeLogConfig(defaultOpsRuntimeLogConfig(nil)) })
	repo := &opsBlockedRefreshRepo{runtimeSettingRepoStub: newRuntimeSettingRepoStub(), read: make(chan struct{}), resume: make(chan struct{})}
	repo.values[SettingKeyOpsRuntimeLogConfig] = `{"persist_access_logs":false}`
	svc := &OpsService{settingRepo: repo, systemLogSink: &OpsSystemLogSink{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	refreshed := make(chan error, 1)
	go func() { refreshed <- svc.RefreshRuntimeSettings(ctx) }()
	select {
	case <-repo.read:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	next := defaultOpsRuntimeLogConfig(nil)
	next.PersistAccessLogs = true
	saved := make(chan error, 1)
	go func() { _, err := svc.UpdateRuntimeLogConfig(ctx, next, 1); saved <- err }()
	// 旧实现会在刷新持有旧快照期间完成保存；新实现等待刷新完成后再保存。
	var early bool
	select {
	case err := <-saved:
		early = true
		if err != nil {
			t.Error(err)
		}
	case <-time.After(30 * time.Millisecond):
	}
	close(repo.resume)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if !early {
		if err := <-saved; err != nil {
			t.Fatal(err)
		}
	}
	if !svc.systemLogSink.persistAccessLogs.Load() {
		t.Fatal("stale refresh overwrote the saved access-log policy")
	}
}
