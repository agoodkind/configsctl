// Package dockerbridge serves one private Docker Engine socket per OpenTofu
// Docker provider alias by running the pveguest dockersocket proxy for the
// guest of that alias.
package dockerbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agoodkind/terraform-provider-pveguest/dockersocket"
	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

// HostsVariable is the name of the ephemeral OpenTofu variable that receives
// the socket address of each alias.
const HostsVariable = "docker_hosts"

const (
	variablePrefix      = "TF_VAR_"
	socketDirectoryName = "docker"
	socketSuffix        = ".sock"
	socketScheme        = "unix://"
	directoryMode       = 0o700

	// guestSocket and maxBodyBytes match the defaults of the pveguest-docker-socket command.
	guestSocket  = "/var/run/docker.sock"
	maxBodyBytes = 256 << 20

	// maxSocketPathBytes permits a terminating NUL byte in the 104-byte
	// sun_path field on macOS.
	maxSocketPathBytes = 103

	startupTimeout      = 10 * time.Second
	startupPollInterval = 20 * time.Millisecond
)

var aliasPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// Bridge identifies the guest of one Docker provider alias and the Proxmox
// API token of that guest. TokenVariable identifies the OpenTofu variable
// for the token secret.
type Bridge struct {
	Node           string `json:"node"`
	NodeName       string `json:"node_name"`
	Endpoint       string `json:"endpoint"`
	Kind           string `json:"kind"`
	TokenPrincipal string `json:"token_principal"`
	TokenVariable  string `json:"token_variable"`
	VMID           int64  `json:"vmid"`
	Insecure       bool   `json:"insecure"`
}

// GuestKey returns the guest key in the <node>/<vmid> form of guest_hosts.
func (b Bridge) GuestKey() string {
	return b.Node + "/" + strconv.FormatInt(b.VMID, 10)
}

// Validate requires the guest identity, a supported guest kind, the API
// endpoint, and both token fields.
func (b Bridge) Validate() error {
	var missing []string
	if b.Node == "" {
		missing = append(missing, "node")
	}
	if b.VMID <= 0 {
		missing = append(missing, "vmid")
	}
	if b.Endpoint == "" {
		missing = append(missing, "endpoint")
	}
	if b.TokenPrincipal == "" {
		missing = append(missing, "token_principal")
	}
	if b.TokenVariable == "" {
		missing = append(missing, "token_variable")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the Docker bridge does not set %s", strings.Join(missing, ", "))
	}
	kind := transport.Kind(b.Kind)
	if kind != transport.KindLXC && kind != transport.KindQEMU {
		return fmt.Errorf("the Docker bridge kind %q must be lxc or qemu", b.Kind)
	}
	return nil
}

// ValidateAlias requires an OpenTofu provider alias name.
func ValidateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return fmt.Errorf("the Docker bridge alias %q is not an OpenTofu provider alias name", alias)
	}
	return nil
}

// Session is the set of running Docker socket proxies of one OpenTofu command.
type Session struct {
	directory     string
	argument      string
	cancelServe   context.CancelFunc
	cancelCommand context.CancelCauseFunc
	stopping      atomic.Bool
	serving       sync.WaitGroup
	mutex         sync.Mutex
	failures      []error
	stopOnce      sync.Once
	stopErr       error
}

type prepared struct {
	alias  string
	socket string
	client *transport.Client
	guest  transport.Guest
}

// Start creates <directory>/docker, starts one proxy on a private socket for
// each alias, and returns after every socket accepts a connection. It reads
// each token secret from TF_VAR_<TokenVariable> in environment. The proxy
// accepts every method when writable maps its alias to true. Every other
// proxy accepts only GET and HEAD requests. The context that Start returns
// ends when ctx ends or when any proxy stops before Stop. The caller calls
// Stop after a nil error.
func Start(
	ctx context.Context, directory string, bridges map[string]Bridge, environment []string, writable map[string]bool,
) (*Session, context.Context, error) {
	if len(bridges) == 0 {
		return nil, nil, errors.New("start Docker bridges: the command does not declare any alias")
	}
	socketDirectory := filepath.Join(directory, socketDirectoryName)
	proxies, sockets, err := prepareProxies(socketDirectory, bridges, environment)
	if err != nil {
		return nil, nil, err
	}
	argument, err := hostsArgument(sockets)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(socketDirectory, directoryMode); err != nil {
		slog.Error("docker_bridge.directory_failed", "directory", socketDirectory, "err", err)
		return nil, nil, fmt.Errorf("create the Docker bridge socket directory: %w", err)
	}
	if err := os.Chmod(socketDirectory, directoryMode); err != nil {
		removeDirectory(socketDirectory)
		slog.Error("docker_bridge.directory_failed", "directory", socketDirectory, "err", err)
		return nil, nil, fmt.Errorf("restrict the Docker bridge socket directory: %w", err)
	}

	serveContext, cancelServe := context.WithCancel(ctx)
	commandContext, cancelCommand := context.WithCancelCause(ctx)
	session := &Session{
		directory: socketDirectory, argument: argument,
		cancelServe: cancelServe, cancelCommand: cancelCommand,
		stopping: atomic.Bool{}, serving: sync.WaitGroup{}, mutex: sync.Mutex{},
		failures: nil, stopOnce: sync.Once{}, stopErr: nil,
	}
	exits := make(map[string]chan struct{}, len(proxies))
	for _, proxy := range proxies {
		exited := make(chan struct{})
		exits[proxy.alias] = exited
		session.serving.Go(func() {
			defer close(exited)
			defer func() {
				if recovered := recover(); recovered != nil {
					session.finish(proxy.alias, fmt.Errorf("the proxy panicked: %v", recovered))
				}
			}()
			session.serve(serveContext, proxy, !writable[proxy.alias])
		})
	}
	for _, proxy := range proxies {
		if err := waitForSocket(ctx, proxy, exits[proxy.alias]); err != nil {
			return nil, nil, errors.Join(err, session.Stop())
		}
	}
	aliases := slices.Sorted(maps.Keys(bridges))
	writableAliases := slices.DeleteFunc(slices.Clone(aliases), func(alias string) bool { return !writable[alias] })
	slog.Info("docker_bridge.started", "aliases", aliases, "writable", writableAliases)
	return session, commandContext, nil
}

// Argument returns the -var=docker_hosts=<map> OpenTofu argument. The map
// assigns each alias the unix:// address of its socket.
func (s *Session) Argument() string {
	return s.argument
}

// Stop cancels every proxy, waits for each one to return, removes the socket
// directory, and returns the joined proxy and removal errors. Later calls
// return the same result.
func (s *Session) Stop() error {
	s.stopOnce.Do(func() {
		s.stopping.Store(true)
		s.cancelServe()
		s.serving.Wait()
		s.cancelCommand(context.Canceled)
		s.mutex.Lock()
		failures := slices.Clone(s.failures)
		s.mutex.Unlock()
		if err := os.RemoveAll(s.directory); err != nil {
			slog.Error("docker_bridge.directory_remove_failed", "directory", s.directory, "err", err)
			failures = append(failures, fmt.Errorf("remove the Docker bridge socket directory: %w", err))
		}
		s.stopErr = errors.Join(failures...)
	})
	return s.stopErr
}

func (s *Session) serve(ctx context.Context, proxy prepared, getOnly bool) {
	options := dockersocket.Options{
		SocketPath: proxy.socket, GuestSocket: guestSocket, GetOnly: getOnly, MaxBodyBytes: maxBodyBytes,
	}
	s.finish(proxy.alias, dockersocket.Serve(ctx, proxy.client, proxy.guest, options))
}

// finish records a proxy error. It also ends the command context when the
// proxy returned before Stop.
func (s *Session) finish(alias string, err error) {
	if err != nil {
		err = fmt.Errorf("the Docker bridge %s: %w", alias, err)
		slog.Error("docker_bridge.stopped", "alias", alias, "err", err)
		s.mutex.Lock()
		s.failures = append(s.failures, err)
		s.mutex.Unlock()
	}
	if s.stopping.Load() {
		return
	}
	if err == nil {
		err = fmt.Errorf("the Docker bridge %s stopped before the OpenTofu command finished", alias)
	}
	s.cancelCommand(err)
}

func prepareProxies(socketDirectory string, bridges map[string]Bridge, environment []string) ([]prepared, map[string]string, error) {
	aliases := slices.Sorted(maps.Keys(bridges))
	proxies := make([]prepared, 0, len(aliases))
	sockets := make(map[string]string, len(aliases))
	for index, alias := range aliases {
		bridge := bridges[alias]
		if err := ValidateAlias(alias); err != nil {
			return nil, nil, err
		}
		if err := bridge.Validate(); err != nil {
			return nil, nil, fmt.Errorf("the Docker bridge %s: %w", alias, err)
		}
		socket := filepath.Join(socketDirectory, strconv.Itoa(index)+socketSuffix)
		if len(socket) > maxSocketPathBytes {
			return nil, nil, fmt.Errorf("the Docker bridge %s: the socket path %s exceeds %d bytes", alias, socket, maxSocketPathBytes)
		}
		tokenName := variablePrefix + bridge.TokenVariable
		secret := environmentValue(environment, tokenName)
		if secret == "" {
			return nil, nil, fmt.Errorf("the Docker bridge %s requires the token secret %s", alias, tokenName)
		}
		client, err := transport.NewClient(map[string]transport.NodeConfig{
			bridge.Node: {
				Endpoint: bridge.Endpoint, APIToken: bridge.TokenPrincipal + "=" + secret,
				Insecure: bridge.Insecure, NodeName: bridge.NodeName, MaxExecRequests: 0,
			},
		}, transport.DefaultMaxRequests)
		if err != nil {
			slog.Error("docker_bridge.client_failed", "alias", alias, "err", err)
			return nil, nil, fmt.Errorf("the Docker bridge %s: create the Proxmox client: %w", alias, err)
		}
		guest := transport.Guest{Node: bridge.Node, VMID: bridge.VMID, Kind: transport.Kind(bridge.Kind)}
		proxies = append(proxies, prepared{alias: alias, socket: socket, client: client, guest: guest})
		sockets[alias] = socket
	}
	return proxies, sockets, nil
}

func hostsArgument(sockets map[string]string) (string, error) {
	hosts := make(map[string]string, len(sockets))
	for alias, socket := range sockets {
		hosts[alias] = socketScheme + socket
	}
	encoded, err := json.Marshal(hosts)
	if err != nil {
		slog.Error("docker_bridge.assignment_failed", "err", err)
		return "", fmt.Errorf("encode %s: %w", HostsVariable, err)
	}
	return "-var=" + HostsVariable + "=" + string(encoded), nil
}

// waitForSocket dials the socket of proxy until a connection succeeds, the
// proxy returns, ctx ends, or startupTimeout passes.
func waitForSocket(ctx context.Context, proxy prepared, exited <-chan struct{}) error {
	deadline, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	var dialer net.Dialer
	ticker := time.NewTicker(startupPollInterval)
	defer ticker.Stop()
	for {
		connection, err := dialer.DialContext(deadline, "unix", proxy.socket)
		if err == nil {
			if closeErr := connection.Close(); closeErr != nil {
				slog.Warn("docker_bridge.probe_close_failed", "alias", proxy.alias, "err", closeErr)
			}
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("the Docker bridge %s stopped during startup", proxy.alias)
		case <-deadline.Done():
			slog.Error("docker_bridge.startup_failed", "alias", proxy.alias, "err", err)
			return fmt.Errorf("the Docker bridge %s did not accept connections on %s: %w", proxy.alias, proxy.socket, context.Cause(deadline))
		case <-ticker.C:
		}
	}
}

func environmentValue(environment []string, name string) string {
	value := ""
	for _, assignment := range environment {
		key, candidate, _ := strings.Cut(assignment, "=")
		if key == name {
			value = candidate
		}
	}
	return value
}

func removeDirectory(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("docker_bridge.directory_remove_failed", "directory", path, "err", err)
	}
}
