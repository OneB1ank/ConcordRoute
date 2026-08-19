package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/mihomo"
	proxynode "github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/node"
	"github.com/TokenFlux/TokenRouter/internal/clashproxy/internal/profile"
)

const defaultStartupTimeout = 12 * time.Second

type NodeStore interface {
	ListProfileNodes(context.Context, int64) ([]proxynode.Node, error)
}

type InstanceStore interface {
	GetRunningRuntime(context.Context, int64) (*Instance, error)
	GetRuntime(context.Context, int64) (*Instance, error)
	SaveRuntimeStarting(context.Context, Instance) error
	MarkRuntimeRunning(context.Context, int64, int) error
	MarkRuntimeFailed(context.Context, int64, string) error
	MarkRuntimeStopped(context.Context, int64) error
}

type processState struct {
	cmd      *exec.Cmd
	stopping bool
}

// ProcessManager 管理 mihomo 子进程，并确保数据库状态只在控制端就绪后进入 running。
type ProcessManager struct {
	BinaryPath       string
	RuntimeRoot      string
	Nodes            NodeStore
	Instances        InstanceStore
	PortAllocator    *PortAllocator
	Controller       ControllerClient
	StartupTimeout   time.Duration
	OnUnexpectedExit func(context.Context, int64, error) error

	mu        sync.Mutex
	processes map[int64]*processState
}

// SetControllerHTTPClient 为控制端就绪探测注入统一的 HTTP 客户端。
func (m *ProcessManager) SetControllerHTTPClient(client *http.Client) {
	if m == nil {
		return
	}
	m.Controller.HTTPClient = client
}

func (m *ProcessManager) EnsureRunning(ctx context.Context, prof profile.Profile) (*Instance, error) {
	if m == nil || m.Nodes == nil || m.Instances == nil {
		return nil, errors.New("clash proxy process manager is not configured")
	}
	if err := prof.ValidateActive(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// ProcessManager may be constructed directly in tests or by future callers
	// instead of through the service factory. Keep the zero value safe so the
	// first profile start does not panic when recording the child process.
	if m.processes == nil {
		m.processes = make(map[int64]*processState)
	}

	if state := m.processes[prof.ID]; state != nil && state.cmd != nil && state.cmd.Process != nil {
		if existing, err := m.Instances.GetRunningRuntime(ctx, prof.ID); err == nil && existing != nil {
			return existing, nil
		}
	}

	binary := strings.TrimSpace(m.BinaryPath)
	if binary == "" {
		return nil, errors.New("mihomo binary path is required")
	}
	if info, err := os.Stat(binary); err != nil {
		return nil, fmt.Errorf("stat mihomo binary: %w", err)
	} else if info.IsDir() {
		return nil, errors.New("mihomo binary path is a directory")
	}

	nodes, err := m.Nodes.ListProfileNodes(ctx, prof.ID)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("proxy profile has no enabled nodes")
	}
	mixedPort, controllerPort, err := m.portAllocator().AllocatePair()
	if err != nil {
		return nil, err
	}
	secret, err := randomSecret()
	if err != nil {
		return nil, err
	}

	workDir := filepath.Join(m.runtimeRoot(), fmt.Sprintf("profile-%d", prof.ID))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create mihomo runtime directory: %w", err)
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure mihomo runtime directory: %w", err)
	}
	configPath := filepath.Join(workDir, "config.yaml")
	configBytes, err := mihomo.Compile(mihomo.Profile{
		ID:              prof.ID,
		Strategy:        prof.Strategy,
		TestURL:         prof.TestURL,
		IntervalSeconds: prof.IntervalSeconds,
	}, nodes, mihomo.RuntimeConfig{
		MixedPort:        mixedPort,
		ControllerPort:   controllerPort,
		ControllerSecret: secret,
	})
	if err != nil {
		return nil, err
	}
	if err := writeConfigAtomically(configPath, configBytes); err != nil {
		return nil, err
	}

	instance := Instance{
		ProfileID:           prof.ID,
		RuntimeType:         "mihomo",
		MixedPort:           mixedPort,
		ControllerPort:      controllerPort,
		ControllerSecretRef: secret,
		ConfigPath:          configPath,
		WorkDir:             workDir,
		Status:              StatusStarting,
	}
	if err := m.Instances.SaveRuntimeStarting(ctx, instance); err != nil {
		return nil, err
	}

	// 子进程生命周期独立于单次 HTTP 请求；请求取消只中断就绪等待并触发清理。
	cmd := exec.Command(binary, "-d", workDir, "-f", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = m.Instances.MarkRuntimeFailed(context.Background(), prof.ID, err.Error())
		return nil, fmt.Errorf("start mihomo: %w", err)
	}
	m.processes[prof.ID] = &processState{cmd: cmd}
	instance.PID = cmd.Process.Pid

	startupTimeout := m.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = defaultStartupTimeout
	}
	readyCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	readyErr := m.Controller.WaitReady(readyCtx, instance, secret)
	cancel()
	if readyErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		delete(m.processes, prof.ID)
		_ = m.Instances.MarkRuntimeFailed(context.Background(), prof.ID, readyErr.Error())
		return nil, fmt.Errorf("mihomo controller readiness check: %w", readyErr)
	}
	if err := m.Instances.MarkRuntimeRunning(ctx, prof.ID, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		delete(m.processes, prof.ID)
		_ = m.Instances.MarkRuntimeFailed(context.Background(), prof.ID, err.Error())
		return nil, err
	}

	instance.Status = StatusRunning
	go m.waitProcess(prof.ID, cmd)
	return &instance, nil
}

func (m *ProcessManager) Stop(profileID int64) error {
	if m == nil || profileID <= 0 {
		return nil
	}
	m.mu.Lock()
	state := m.processes[profileID]
	if state != nil {
		state.stopping = true
	}
	m.mu.Unlock()

	var killErr error
	if state != nil && state.cmd != nil && state.cmd.Process != nil {
		killErr = state.cmd.Process.Kill()
	}
	if m.Instances != nil {
		if err := m.Instances.MarkRuntimeStopped(context.Background(), profileID); err != nil && killErr == nil {
			killErr = err
		}
	}
	return killErr
}

func (m *ProcessManager) StopAll() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	ids := make([]int64, 0, len(m.processes))
	for id := range m.processes {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	var errs []error
	for _, id := range ids {
		if err := m.Stop(id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *ProcessManager) waitProcess(profileID int64, cmd *exec.Cmd) {
	err := cmd.Wait()
	m.mu.Lock()
	state := m.processes[profileID]
	stopping := state != nil && state.cmd == cmd && state.stopping
	if state != nil && state.cmd == cmd {
		delete(m.processes, profileID)
	}
	m.mu.Unlock()

	m.recordProcessExit(profileID, stopping, err)
}

func (m *ProcessManager) recordProcessExit(profileID int64, stopping bool, processErr error) {
	if m.Instances == nil {
		return
	}
	if stopping {
		_ = m.Instances.MarkRuntimeStopped(context.Background(), profileID)
		return
	}
	failure := errors.New("mihomo exited unexpectedly")
	if processErr != nil {
		failure = processErr
	}
	if m.OnUnexpectedExit != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cleanupErr := m.OnUnexpectedExit(ctx, profileID, failure)
		cancel()
		if cleanupErr != nil {
			failure = errors.Join(failure, fmt.Errorf("restore account bindings: %w", cleanupErr))
		}
	}
	_ = m.Instances.MarkRuntimeFailed(context.Background(), profileID, failure.Error())
}

func (m *ProcessManager) portAllocator() *PortAllocator {
	if m.PortAllocator == nil {
		m.PortAllocator = NewPortAllocator()
	}
	return m.PortAllocator
}

func (m *ProcessManager) runtimeRoot() string {
	if root := strings.TrimSpace(m.RuntimeRoot); root != "" {
		return root
	}
	return filepath.Join("data", "clash-proxy")
}

func writeConfigAtomically(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return fmt.Errorf("write mihomo config: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("secure mihomo config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("activate mihomo config: %w", err)
	}
	return nil
}

func randomSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
