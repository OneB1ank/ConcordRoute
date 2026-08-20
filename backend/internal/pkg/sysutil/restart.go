// Package sysutil provides system-level utilities for process management.
package sysutil

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"
)

const serviceName = "sub2api"

// findExecutable resolves a command when PATH is restricted by a service manager.
func findExecutable(name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	for _, path := range []string{"/usr/bin/" + name, "/bin/" + name, "/usr/sbin/" + name, "/sbin/" + name} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return name
}

func startDetachedSystemdRestart() error {
	systemctl := findExecutable("systemctl")
	setsid := findExecutable("setsid")
	args := []string{systemctl, "restart", serviceName}
	if os.Geteuid() != 0 {
		args = []string{findExecutable("sudo"), "-n"}
		args = append(args, systemctl, "restart", serviceName)
	}
	cmd := exec.Command(setsid, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached restart: %w", err)
	}
	return nil
}

func containerSupervisorPresent() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return os.Getppid() == 1
}

// RestartService requests a restart without taking down an unmanaged process.
// A direct exit is used only when PID 1/container supervision will restart it;
// systemd-managed services use a detached systemctl request instead.
func RestartService() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("service restart is supported on Linux only")
	}

	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" {
		if err := startDetachedSystemdRestart(); err == nil {
			log.Println("systemd restart request initiated")
			return nil
		} else {
			return fmt.Errorf("systemd restart request failed: %w", err)
		}
	}

	if !containerSupervisorPresent() {
		return fmt.Errorf("no restart supervisor detected; restart the service with systemctl or the container runtime")
	}

	log.Println("Container supervisor detected; exiting for restart")

	// Give a moment for logs to flush and response to be sent
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(0)
	}()

	return nil
}

// RestartServiceAsync is a fire-and-forget version of RestartService.
// It logs errors instead of returning them, suitable for goroutine usage.
func RestartServiceAsync() {
	if err := RestartService(); err != nil {
		log.Printf("Service restart failed: %v", err)
		log.Println("Please restart the service manually: sudo systemctl restart sub2api")
	}
}
