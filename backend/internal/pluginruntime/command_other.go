//go:build !windows

package pluginruntime

import "os/exec"

func hideCommandWindow(*exec.Cmd) {}
