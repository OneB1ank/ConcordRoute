package runtime

import (
	"errors"
	"fmt"
)

type Status string

const (
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusFailed   Status = "failed"
	StatusStopped  Status = "stopped"
)

type Instance struct {
	ID                  int64
	ProfileID           int64
	RuntimeType         string
	PID                 int
	MixedPort           int
	ControllerPort      int
	ControllerSecretRef string
	ConfigPath          string
	WorkDir             string
	Status              Status
	LastError           string
}

func (i Instance) ProxyURL() (string, error) {
	if i.ProfileID <= 0 {
		return "", errors.New("runtime profile id is required")
	}
	if i.Status != StatusRunning {
		return "", errors.New("proxy runtime is not running")
	}
	if i.MixedPort <= 0 || i.MixedPort > 65535 {
		return "", errors.New("proxy runtime mixed port is invalid")
	}
	return fmt.Sprintf("http://127.0.0.1:%d", i.MixedPort), nil
}
