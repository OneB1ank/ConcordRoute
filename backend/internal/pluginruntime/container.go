// Package pluginruntime contains host-owned process/container lifecycle adapters.
package pluginruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-plugin/runner"
)

type ContainerOptions struct {
	BinaryPath   string
	BinarySHA256 string
	WorkDir      string
	Image        string
	MemoryMB     int
	CPUMilli     int
	PidsLimit    int
	Env          []string
}

type Container struct {
	options     ContainerOptions
	docker      string
	name        string
	id          string
	attach      *exec.Cmd
	stdout      io.ReadCloser
	stderr      io.ReadCloser
	lease       *os.File
	cancel      context.CancelFunc
	ctx         context.Context
	stop        sync.Once
	mu          sync.Mutex
	closed      bool
	listeners   map[string]net.Listener
	connections map[net.Conn]struct{}
	slots       chan struct{}
}

var dockerIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func NewContainer(options ContainerOptions) (*Container, error) {
	if options.MemoryMB < 64 || options.MemoryMB > 4096 || options.CPUMilli < 100 || options.CPUMilli > 4000 || options.PidsLimit < 32 || options.PidsLimit > 256 {
		return nil, errors.New("invalid sandbox resource limits")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return nil, errors.New("container isolation requires a local Docker Engine")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Container{options: options, docker: docker, name: "sub2api-extension-" + hex.EncodeToString(nonce),
		ctx: ctx, cancel: cancel, listeners: map[string]net.Listener{}, connections: map[net.Conn]struct{}{}, slots: make(chan struct{}, 4)}, nil
}

func (c *Container) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.docker, args...)
	hideCommandWindow(cmd)
	return cmd
}
func (c *Container) Start(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			_ = c.Kill(context.Background())
		}
	}()
	if explicit := os.Getenv("DOCKER_HOST"); explicit != "" && !localDockerEndpoint(explicit) {
		return errors.New("sandbox requires a local Docker endpoint")
	}
	endpoint, err := c.command(ctx, "context", "inspect", "--format", "{{json .Endpoints.docker.Host}}").Output()
	if err != nil {
		return fmt.Errorf("inspect local Docker context: %w", err)
	}
	var address string
	if json.Unmarshal(endpoint, &address) != nil || !localDockerEndpoint(address) {
		return errors.New("sandbox requires a local Docker context")
	}
	raw, err := c.command(ctx, "image", "inspect", c.options.Image).Output()
	if err != nil {
		return errors.New("sandbox image is not installed; build deploy/Dockerfile.plugin-sandbox first")
	}
	var images []struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string
		Config       struct {
			Labels  map[string]string
			Volumes map[string]json.RawMessage
		}
	}
	if json.Unmarshal(raw, &images) != nil || len(images) != 1 || images[0].OS != "linux" || images[0].Architecture != runtime.GOARCH ||
		images[0].Config.Labels["org.sub2api.plugin-sandbox.api"] != "1" || len(images[0].Config.Volumes) > 0 || !strings.HasPrefix(images[0].ID, "sha256:") {
		return errors.New("sandbox image has an incompatible platform or supervisor API")
	}
	if err = c.stageBinary(); err != nil {
		return err
	}
	leasePath := filepath.Join(c.options.WorkDir, "lease")
	c.lease, err = os.OpenFile(leasePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if _, err = c.lease.WriteAt([]byte("0000000000000000"), 0); err != nil {
		return err
	}
	if err = c.lease.Chmod(0444); err != nil {
		return err
	}
	_ = c.lease.Sync()
	args, err := c.createArgs(images[0].ID, leasePath)
	if err != nil {
		return err
	}
	created, err := c.command(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create isolated plugin: %w (%s)", err, boundedMessage(created))
	}
	c.id = strings.TrimSpace(string(created))
	if !dockerIDPattern.MatchString(c.id) {
		return errors.New("docker returned an invalid container identifier")
	}
	c.attach = c.command(c.ctx, "start", "--attach", c.id)
	c.stdout, err = c.attach.StdoutPipe()
	if err != nil {
		return err
	}
	c.stderr, err = c.attach.StderrPipe()
	if err != nil {
		return err
	}
	if err = c.attach.Start(); err != nil {
		return err
	}
	go c.renewLease()
	return nil
}

func localDockerEndpoint(endpoint string) bool {
	return strings.HasPrefix(endpoint, "unix://") || strings.HasPrefix(strings.ToLower(endpoint), "npipe:////./pipe/")
}
func boundedMessage(raw []byte) string {
	if len(raw) > 2048 {
		raw = raw[:2048]
	}
	return strings.TrimSpace(string(raw))
}
func (c *Container) stageBinary() error {
	source, err := os.Open(c.options.BinaryPath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile(filepath.Join(c.options.WorkDir, "runtime"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0555)
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(target, digest), source)
	closeErr := target.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(digest.Sum(nil)) != c.options.BinarySHA256 {
		return errors.New("sandbox binary checksum mismatch")
	}
	return nil
}
func mountArgument(source, target string) (string, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	if strings.ContainsAny(absolute, ",\r\n\x00") {
		return "", errors.New("sandbox source paths cannot contain commas or control characters")
	}
	return "type=bind,src=" + absolute + ",dst=" + target + ",readonly", nil
}
func (c *Container) createArgs(imageID, leasePath string) ([]string, error) {
	binaryMount, err := mountArgument(filepath.Join(c.options.WorkDir, "runtime"), "/plugin/runtime")
	if err != nil {
		return nil, err
	}
	leaseMount, err := mountArgument(leasePath, "/lease")
	if err != nil {
		return nil, err
	}
	memory := strconv.Itoa(c.options.MemoryMB) + "m"
	args := []string{"create", "--pull=never", "--rm", "--name", c.name, "--label", "org.sub2api.plugin.owner=" + c.name,
		"--user", "0:0", "--no-healthcheck", "--entrypoint", "/sub2api-plugin-sandbox",
		"--platform", "linux/" + runtime.GOARCH, "--network", "none", "--read-only", "--cap-drop", "ALL", "--cap-add", "SETUID", "--cap-add", "SETGID",
		"--security-opt", "no-new-privileges=true", "--pids-limit", strconv.Itoa(c.options.PidsLimit), "--memory", memory, "--memory-swap", memory,
		"--cpus", fmt.Sprintf("%.3f", float64(c.options.CPUMilli)/1000), "--ulimit", "nofile=256:256",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216,mode=1777",
		"--tmpfs", "/rpc:rw,noexec,nosuid,nodev,size=1048576,mode=0700,uid=65532,gid=65532",
		"--mount", binaryMount, "--mount", leaseMount}
	allowed := map[string]bool{"SUB2API_PLUGIN_MAGIC_COOKIE": true, "PLUGIN_PROTOCOL_VERSIONS": true, "PLUGIN_CLIENT_CERT": true,
		"PLUGIN_MULTIPLEX_GRPC": true, "PLUGIN_MIN_PORT": true, "PLUGIN_MAX_PORT": true}
	for _, entry := range c.options.Env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed[key] {
			args = append(args, "--env", entry)
		}
	}
	args = append(args, "--env", "PLUGIN_UNIX_SOCKET_DIR=/rpc", "--env", "HOME=/tmp", "--env", "TMPDIR=/tmp", "--env", "GOMAXPROCS=2", imageID, "supervise")
	return args, nil
}
func (c *Container) renewLease() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			sequence++
			if _, err := c.lease.WriteAt([]byte(fmt.Sprintf("%016x", sequence)), 0); err != nil {
				return
			}
			_ = c.lease.Sync()
		}
	}
}
func (c *Container) Stdout() io.ReadCloser { return c.stdout }
func (c *Container) Stderr() io.ReadCloser { return c.stderr }
func (c *Container) Name() string          { return c.name }
func (c *Container) ID() string            { return c.id }
func (c *Container) Diagnose(context.Context) string {
	return "isolated plugin failed; verify the local sandbox image, Linux runtime, and resource limits"
}
func (c *Container) Wait(context.Context) error {
	if c.attach == nil {
		return nil
	}
	err := c.attach.Wait()
	_ = c.Kill(context.Background())
	return err
}
func (c *Container) Kill(context.Context) error {
	c.stop.Do(func() {
		c.cancel()
		c.mu.Lock()
		c.closed = true
		for _, listener := range c.listeners {
			_ = listener.Close()
		}
		for conn := range c.connections {
			_ = conn.Close()
		}
		c.mu.Unlock()
		if c.lease != nil {
			_ = c.lease.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		target := c.id
		if !dockerIDPattern.MatchString(target) {
			// Creation may have completed after cancellation. Only remove the
			// uniquely named resource when its ownership label matches.
			raw, err := c.command(ctx, "inspect", "--format", `{{index .Config.Labels "org.sub2api.plugin.owner"}}`, c.name).Output()
			if err != nil || strings.TrimSpace(string(raw)) != c.name {
				return
			}
			target = c.name
		}
		_ = c.command(ctx, "rm", "--force", target).Run()
	})
	return nil
}
func (c *Container) PluginToHost(network, address string) (string, string, error) {
	if network != "unix" || path.Clean(address) != address || !strings.HasPrefix(address, "/rpc/") || len(address) > 108 {
		return "", "", errors.New("sandbox advertised an unauthorized RPC address")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", "", errors.New("sandbox is stopped")
	}
	if listener := c.listeners[address]; listener != nil {
		return "tcp", listener.Addr().String(), nil
	}
	if len(c.listeners) >= 4 {
		return "", "", errors.New("sandbox RPC listener limit exceeded")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
	}
	c.listeners[address] = listener
	go c.accept(listener, address)
	return "tcp", listener.Addr().String(), nil
}
func (c *Container) HostToPlugin(network, address string) (string, string, error) {
	return "", "", errors.New("sandbox host callbacks require a multiplexed broker")
}
func (c *Container) accept(listener net.Listener, socket string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case c.slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			_ = conn.Close()
			<-c.slots
			return
		}
		c.connections[conn] = struct{}{}
		c.mu.Unlock()
		go func() {
			defer func() { _ = conn.Close(); c.mu.Lock(); delete(c.connections, conn); c.mu.Unlock(); <-c.slots }()
			ctx, cancel := context.WithCancel(c.ctx)
			defer cancel()
			bridge := c.command(ctx, "exec", "--interactive", "--user", "65532:65532", c.id, "/sub2api-plugin-sandbox", "bridge", socket)
			bridge.Stdin, bridge.Stdout = conn, conn
			bridge.Stderr = io.Discard
			_ = bridge.Run()
		}()
	}
}

var _ runner.Runner = (*Container)(nil)
