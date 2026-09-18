package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"reflect"
	"runtime"
	"time"
)

func (m *PluginManager) cleanupRetiredInstallationFiles(installation *PluginInstallation) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		err = m.cleanupInstallationFiles(installation)
		if err == nil || runtime.GOOS != "windows" || !errors.Is(err, fs.ErrPermission) {
			return err
		}
		// Windows can briefly retain the executable handle after process exit.
		time.Sleep(100 * time.Millisecond)
	}
	return err
}

// PluginVersion lists rollback candidates without disclosing package/config data.
type PluginVersion struct {
	ID            int64               `json:"id"`
	PluginID      int64               `json:"plugin_id"`
	Version       string              `json:"version"`
	BinarySHA256  string              `json:"binary_sha256"`
	SavedAt       time.Time           `json:"saved_at"`
	ExpiresAt     time.Time           `json:"expires_at"`
	Manifest      PluginManifest      `json:"manifest"`
	Compatibility PluginCompatibility `json:"compatibility"`
}

// Version writes use a transaction and an optimistic snapshot so simultaneous
// configuration/enable/upgrade operations on other hosts cannot be overwritten.
type PluginVersionRepository interface {
	ListVersions(context.Context, int64) ([]PluginVersion, error)
	GetVersion(context.Context, int64, int64) (*PluginInstallation, error)
	SwapVersion(context.Context, *PluginInstallation, *PluginInstallation) (*PluginInstallation, error)
}

func (m *PluginManager) canKeepRuntimeDuringReplacement(ctx context.Context, current *pluginRuntime, target *PluginInstallation) bool {
	if current == nil || current.client == nil || current.client.Exited() || current.draining.Load() {
		return false
	}
	if current.installation.Manifest.SchemaVersion != target.Manifest.SchemaVersion ||
		!reflect.DeepEqual(current.installation.Manifest.SortedCapabilities(), target.Manifest.SortedCapabilities()) ||
		!samePluginBindings(current.installation.Bindings, target.Bindings) {
		return false
	}
	// A different package may fail to start on one replica. Retain the old
	// healthy package only when the authoritative grants/scopes are identical.
	if current.installation.BinarySHA256 == target.BinarySHA256 {
		return false
	}
	healthCtx, cancel := context.WithTimeout(ctx, pluginHealthTimeout)
	defer cancel()
	return current.checkHealth(healthCtx) == nil
}

func (m *PluginManager) ListVersions(ctx context.Context, id int64) ([]PluginVersion, error) {
	repo, ok := m.repo.(PluginVersionRepository)
	if !ok {
		return nil, errors.New("插件版本存储不可用")
	}
	versions, err := repo.ListVersions(ctx, id)
	if err != nil {
		return nil, err
	}
	for i := range versions {
		versions[i].Compatibility = EvaluatePluginCompatibility(versions[i].Manifest, m.hostInfo)
	}
	return versions, nil
}

func (m *PluginManager) Upgrade(ctx context.Context, id int64, reader io.Reader, installedBy *int64, acceptUntested bool) (*PluginInstallation, error) {
	return m.UpgradeWithApproval(ctx, id, reader, installedBy, acceptUntested, nil)
}

func (m *PluginManager) UpgradeWithApproval(ctx context.Context, id int64, reader io.Reader, installedBy *int64, acceptUntested bool, approval *PluginPublisherApproval) (*PluginInstallation, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	current, err := m.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	candidate, err := m.installer.InstallWithApproval(ctx, reader, installedBy, approval)
	if err != nil {
		return nil, err
	}
	candidate.ConfigEncrypted = current.ConfigEncrypted
	committed := false
	defer func() {
		if !committed {
			_ = m.cleanupInstallationFiles(candidate)
		}
	}()
	result, err := m.replaceVersion(ctx, current, candidate, acceptUntested)
	if err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func (m *PluginManager) Rollback(ctx context.Context, id, versionID int64, acceptUntested bool) (*PluginInstallation, error) {
	m.operationMu.Lock()
	defer m.operationMu.Unlock()
	repo, ok := m.repo.(PluginVersionRepository)
	if !ok {
		return nil, errors.New("插件版本存储不可用")
	}
	current, err := m.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot, err := repo.GetVersion(ctx, id, versionID)
	if err != nil {
		return nil, fmt.Errorf("读取回滚版本: %w", err)
	}
	candidate, err := m.installer.Install(ctx, bytes.NewReader(snapshot.ArtifactData), snapshot.InstalledBy)
	if err != nil {
		return nil, fmt.Errorf("复验回滚插件包: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = m.cleanupInstallationFiles(candidate)
		}
	}()
	if !samePluginPackage(candidate, snapshot) {
		return nil, errors.New("回滚快照与签名包不一致")
	}
	// Rollback restores the previous version's validated encrypted configuration.
	candidate.ConfigEncrypted = snapshot.ConfigEncrypted
	result, err := m.replaceVersion(ctx, current, candidate, acceptUntested)
	if err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func (m *PluginManager) replaceVersion(ctx context.Context, current, candidate *PluginInstallation, acceptUntested bool) (*PluginInstallation, error) {
	repo, ok := m.repo.(PluginVersionRepository)
	if !ok {
		return nil, errors.New("插件版本存储不可用")
	}
	if current.State == PluginStateStarting {
		return nil, ErrPluginStateChanged
	}
	if candidate.PluginKey != current.PluginKey {
		return nil, errors.New("升级包必须具有相同插件 ID")
	}
	// Existing grants and binding scopes remain valid throughout a hot switch.
	// Contract/permission changes use the disabled installation workflow.
	if current.Manifest.SchemaVersion != candidate.Manifest.SchemaVersion ||
		!reflect.DeepEqual(current.Manifest.SortedCapabilities(), candidate.Manifest.SortedCapabilities()) {
		return nil, errors.New("热切换要求能力、权限和作用域契约保持一致；请停用后安装契约变更包")
	}
	compatibility := EvaluatePluginCompatibility(candidate.Manifest, m.hostInfo)
	if !compatibility.Compatible {
		return nil, errors.New(compatibility.Message)
	}
	if !compatibility.Tested && !acceptUntested {
		return nil, errors.New("目标插件版本未声明已测试当前宿主，需要确认后切换")
	}
	if candidate.BinarySHA256 == current.BinarySHA256 && candidate.Version == current.Version {
		return nil, errors.New("目标包已是当前运行版本")
	}
	candidate.ID = current.ID
	candidate.State = PluginStateDisabled
	candidate.Bindings = append([]PluginBinding(nil), current.Bindings...)
	if hasEnabledPluginBinding(candidate.Bindings) {
		candidate.State = PluginStateEnabled
	}
	process, err := m.prepareRuntime(ctx, candidate, true)
	if err != nil {
		return nil, fmt.Errorf("目标版本验证失败，当前版本保持不变: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			process.kill()
		}
	}()
	checkCtx, cancel := context.WithTimeout(ctx, pluginHealthTimeout)
	err = process.checkHealth(checkCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	// No route changes until the database has saved the old snapshot and
	// atomically replaced the current package under its expected revision.
	var replaced *PluginInstallation
	if candidate.PublisherToTrust != nil {
		if publishers, ok := m.repo.(PluginPublisherVersionRepository); ok {
			replaced, err = publishers.SwapVersionWithPublisher(ctx, current, candidate, candidate.PublisherToTrust)
		} else {
			err = errors.New("发布者信任存储不可用")
		}
	} else {
		replaced, err = repo.SwapVersion(ctx, current, candidate)
	}
	if err != nil {
		return nil, err
	}
	local := mergeLocalInstallation(candidate, replaced)
	m.mu.Lock()
	m.localInstallations[current.ID] = local
	if hasEnabledPluginBinding(replaced.Bindings) {
		process.installation = local
		m.publishRuntimeLocked(local, process)
		keep = true
	}
	m.mu.Unlock()
	replaced.Compatibility = compatibility
	replaced.RuntimeHealthy = keep
	if keep {
		replaced.RuntimeVersion = local.Version
		replaced.RuntimeIsolation = process.isolation
	}
	if keep {
		replaced.RuntimeMessage = "目标版本已通过健康检查并接管新请求"
	}
	return replaced, nil
}
