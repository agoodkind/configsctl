package hostlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Request selects one lock operation for a run.
type Request struct {
	Operation       Operation
	Run, Controller string
	Lease           time.Duration
}

// Response reports the server clock and lock expiry.
type Response struct {
	HostTime, ExpiresAt time.Time
}

// OperationTransport executes the existing host lock protocol.
type OperationTransport interface {
	Do(context.Context, Host, Request) (Response, error)
}

// Options selects transport, renewal failure policy, and operation deadline.
type Options struct {
	Transport      OperationTransport
	StrictRenewal  bool
	RequestTimeout time.Duration
}

func (s *Set) operation(ctx context.Context, host Host, request command) (answer, error) {
	if s.options.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.options.RequestTimeout)
		defer cancel()
	}
	if s.options.Transport == nil {
		return runScript(ctx, host, request)
	}
	response, err := s.options.Transport.Do(ctx, host, Request{
		Operation: request.op, Run: request.run, Controller: request.controller, Lease: request.lease,
	})
	if err != nil {
		return noLease(), err
	}
	if request.op != Release && (!response.ExpiresAt.After(response.HostTime) || response.HostTime.IsZero()) {
		return noLease(), errors.New("host lock transport returned an invalid lease")
	}
	return answer{expiry: response.ExpiresAt, remaining: response.ExpiresAt.Sub(response.HostTime)}, nil
}

// RestrictedSSHTransport invokes the administrator's fixed SSH command.
// SSH configuration selects the server principal and executable.
type RestrictedSSHTransport struct{}

// Do sends one bounded request through the fixed SSH command.
func (RestrictedSSHTransport) Do(ctx context.Context, host Host, request Request) (Response, error) {
	args, err := restrictedSSHArgs(host)
	if err != nil {
		return Response{}, err
	}
	message := wireRequest{Version: protocolVersion, Operation: request.Operation, Run: request.Run, LeaseSeconds: int64(request.Lease / time.Second)}
	if err := message.validate(); err != nil {
		return Response{}, err
	}
	body, err := json.Marshal(message)
	if err != nil {
		slog.Error("hostlock.request_encode_failed", "host", host.Name, "err", err)
		return Response{}, fmt.Errorf("encode host lock request: %w", err)
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.WaitDelay = exitWait
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		slog.Error("hostlock.restricted_ssh_failed", "host", host.Name, "operation", request.Operation, "err", err)
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("restricted host lock operation ended: %w", context.Cause(ctx))
		}
		return Response{}, fmt.Errorf("restricted host lock SSH command failed: %w", err)
	}
	var response wireResponse
	if err := unmarshalStrict(stdout.Bytes(), &response); err != nil {
		return Response{}, errors.New("restricted host lock returned an invalid response")
	}
	switch response.Status {
	case statusOK:
		return Response{HostTime: time.Unix(response.HostTime, 0), ExpiresAt: time.Unix(response.ExpiresAt, 0)}, nil
	case statusHeld:
		return Response{}, &HeldError{Host: host.Name, Expiry: time.Unix(response.ExpiresAt, 0)}
	case statusNotHeld:
		return Response{}, ErrNotHeld
	case statusFailed:
		return Response{}, errors.New("restricted host lock refused the operation")
	default:
		return Response{}, errors.New("restricted host lock refused the operation")
	}
}

func restrictedSSHArgs(host Host) ([]string, error) {
	if host.Address == "" || host.User == "" || host.PrivateKeyFile == "" || host.KnownHostsFile == "" {
		return nil, errors.New("restricted host locks require address, user, private key file, and known hosts file")
	}
	if host.Dir != "" {
		return nil, errors.New("restricted host locks do not accept a lock directory")
	}
	for _, path := range []string{host.PrivateKeyFile, host.KnownHostsFile} {
		info, err := os.Stat(path)
		if err != nil || !filepath.IsAbs(path) || !info.Mode().IsRegular() {
			return nil, errors.New("restricted host lock key and known hosts paths must identify existing absolute regular files")
		}
	}
	address := host.Address
	port := "22"
	if host.ConnectAddress != "" {
		address = host.ConnectAddress
	}
	if hostname, selectedPort, err := net.SplitHostPort(address); err == nil {
		address, port = hostname, selectedPort
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return nil, errors.New("restricted host lock SSH port is invalid")
	}
	if strings.ContainsAny(address+host.Address+host.User, "\r\n\t ") || strings.HasPrefix(address, "-") || strings.HasPrefix(host.User, "-") {
		return nil, errors.New("restricted host lock SSH endpoint is invalid")
	}
	args := append([]string{}, sshOptions...)
	args = append(args, "-F", "/dev/null", "-T", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "ClearAllForwardings=yes", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias="+host.Address, "-o", "UserKnownHostsFile="+host.KnownHostsFile,
		"-i", host.PrivateKeyFile, "-p", port, "-l", host.User, address)
	return checkedArgs(args)
}
