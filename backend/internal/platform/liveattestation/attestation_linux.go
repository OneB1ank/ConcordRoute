//go:build linux

package liveattestation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	linuxAttestationHelperEnv = "CONCORDROUTE_LIVE_ATTESTATION_HELPER"
	linuxAttestationTimeout   = 5 * time.Second
	linuxAttestationMaxOutput = 16 << 10
)

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b == nil || len(b.Bytes())+len(p) > b.limit {
		return 0, errors.New("attestation helper output exceeds size limit")
	}
	return b.Buffer.Write(p)
}

// linuxProvider 只桥接外部真实证明提供器，不在网关内生成或伪造 token。
// helper 必须是单一可执行文件，stdout 只能返回 v1 envelope JSON。
type linuxProvider struct {
	helperPath string
}

// NewProvider 在 Linux 上读取外部证明 helper；未配置时保持明确的不可用状态。
func NewProvider() Provider {
	return &linuxProvider{helperPath: strings.TrimSpace(os.Getenv(linuxAttestationHelperEnv))}
}

func (p *linuxProvider) Check(ctx context.Context) error {
	if p == nil || strings.TrimSpace(p.helperPath) == "" {
		return ErrLinuxProviderMissing
	}
	if !filepath.IsAbs(p.helperPath) {
		return fmt.Errorf("%w: helper path must be absolute", ErrLinuxProviderInvalid)
	}
	lstat, err := os.Lstat(p.helperPath)
	if err != nil || !lstat.Mode().IsRegular() || lstat.Mode()&0111 == 0 {
		return fmt.Errorf("%w: %s", ErrLinuxProviderInvalid, p.helperPath)
	}
	if lstat.Mode().Perm()&022 != 0 {
		return fmt.Errorf("%w: helper must not be group/other writable", ErrLinuxProviderInvalid)
	}
	if stat, ok := lstat.Sys().(*syscall.Stat_t); ok {
		euid := uint32(os.Geteuid())
		if stat.Uid != 0 && stat.Uid != euid {
			return fmt.Errorf("%w: helper owner is not root or the service user", ErrLinuxProviderInvalid)
		}
	}
	return nil
}

func (p *linuxProvider) Generate(ctx context.Context) (string, error) {
	if err := p.Check(ctx); err != nil {
		return "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, linuxAttestationTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, p.helperPath)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"CONCORDROUTE_ATTESTATION_OPERATION=generate",
		"CONCORDROUTE_ATTESTATION_PLATFORM=linux",
	}
	stdout := boundedBuffer{limit: linuxAttestationMaxOutput}
	stderr := boundedBuffer{limit: linuxAttestationMaxOutput}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", errors.New("linux attestation helper timed out")
		}
		reason := strings.TrimSpace(stderr.String())
		if len(reason) > 240 {
			reason = reason[:240]
		}
		if reason == "" {
			reason = err.Error()
		}
		return "", fmt.Errorf("linux attestation helper failed: %s", reason)
	}
	header, err := NormalizeClientEnvelope(strings.TrimSpace(stdout.String()))
	if err != nil {
		return "", fmt.Errorf("linux attestation helper returned invalid envelope: %w", err)
	}
	return header, nil
}
