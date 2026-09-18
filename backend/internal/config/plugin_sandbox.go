package config

import (
	"errors"
	"strings"
)

func (c PluginSandboxConfig) WithDefaults() PluginSandboxConfig {
	if c.Mode == "" {
		c.Mode = "process"
	}
	if c.Image == "" {
		c.Image = "sub2api-plugin-sandbox:1"
	}
	if c.MemoryMB == 0 {
		c.MemoryMB = 256
	}
	if c.CPUMilli == 0 {
		c.CPUMilli = 1000
	}
	if c.PidsLimit == 0 {
		c.PidsLimit = 64
	}
	return c
}
func (c PluginSandboxConfig) Validate() error {
	c = c.WithDefaults()
	if c.Mode != "process" && c.Mode != "container" {
		return errors.New("plugins.v2_sandbox.mode must be process or container")
	}
	if strings.TrimSpace(c.Image) != c.Image || strings.HasPrefix(c.Image, "-") || strings.ContainsAny(c.Image, "\x00\r\n ") {
		return errors.New("plugins.v2_sandbox.image is invalid")
	}
	if c.MemoryMB < 64 || c.MemoryMB > 4096 {
		return errors.New("plugins.v2_sandbox.memory_mb must be between 64 and 4096")
	}
	if c.CPUMilli < 100 || c.CPUMilli > 4000 {
		return errors.New("plugins.v2_sandbox.cpu_milli must be between 100 and 4000")
	}
	if c.PidsLimit < 32 || c.PidsLimit > 256 {
		return errors.New("plugins.v2_sandbox.pids_limit must be between 32 and 256")
	}
	return nil
}
