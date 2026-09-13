//go:build linux

package liveattestation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxProviderRequiresConfiguredHelper(t *testing.T) {
	t.Setenv(linuxAttestationHelperEnv, "")
	provider := NewProvider()
	if err := provider.Check(context.Background()); err != ErrLinuxProviderMissing {
		t.Fatalf("Check() error = %v, want ErrLinuxProviderMissing", err)
	}
}

func TestLinuxProviderBridgesExternalEnvelope(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "attestation-helper.sh")
	content := "#!/bin/sh\nprintf '%s' '{\"v\":1,\"s\":0,\"t\":\"v1.linux-helper\"}'\n"
	if err := os.WriteFile(helper, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linuxAttestationHelperEnv, helper)
	provider := NewProvider()
	if err := provider.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	header, err := provider.Generate(context.Background())
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if header != `{"v":1,"s":0,"t":"v1.linux-helper"}` {
		t.Fatalf("Generate() header = %s", header)
	}
}
