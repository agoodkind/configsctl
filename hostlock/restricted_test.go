package hostlock_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/hostlock"
)

// The test subprocess uses the production endpoint with an isolated lock directory.
func TestRestrictedEndpointProcess(t *testing.T) {
	for index, argument := range os.Args {
		if argument == "--host-lock-test" {
			err := (hostlock.Server{Principal: os.Args[index+1], Directory: os.Args[index+2]}).Serve(context.Background(), os.Stdin, os.Stdout)
			if err != nil {
				t.Fatal(err)
			}
			os.Exit(0)
		}
	}
}

func TestRestrictedSSHUsesPinnedIdentityAndSharedLocks(t *testing.T) {
	host, directory := restrictedServer(t, "planning")
	transport := hostlock.RestrictedSSHTransport{}
	request := hostlock.Request{Operation: hostlock.Acquire, Run: "attempt-one", Controller: "runner", Lease: 30 * time.Second}
	if _, err := transport.Do(t.Context(), host, request); err != nil {
		t.Fatal(err)
	}
	legacy := hostlock.Host{Name: "legacy", Dir: directory}
	if err := hostlock.Do(t.Context(), legacy, hostlock.Acquire, "legacy-run", "local"); err == nil {
		t.Fatal("the legacy client acquired the restricted client's lock")
	}
	request.Operation, request.Run = hostlock.Release, "wrong-owner"
	if _, err := transport.Do(t.Context(), host, request); !errors.Is(err, hostlock.ErrNotHeld) {
		t.Fatalf("wrong-owner release: %v", err)
	}
	request.Run = "attempt-one"
	if _, err := transport.Do(t.Context(), host, request); err != nil {
		t.Fatal(err)
	}
	if err := hostlock.Do(t.Context(), legacy, hostlock.Acquire, "legacy-run", "local"); err != nil {
		t.Fatal(err)
	}
	request.Operation = hostlock.Acquire
	if _, err := transport.Do(t.Context(), host, request); err == nil {
		t.Fatal("the restricted client acquired the legacy client's lock")
	}
	wrongIdentity := host
	wrongIdentity.Address = "untrusted.example.invalid"
	if _, err := transport.Do(t.Context(), wrongIdentity, request); err == nil {
		t.Fatal("SSH accepted an unpinned endpoint identity")
	}
	wrongKey := host
	wrongKey.PrivateKeyFile = filepath.Join(t.TempDir(), "missing-key")
	if _, err := transport.Do(t.Context(), wrongKey, request); err == nil {
		t.Fatal("SSH accepted a missing client key")
	}
}

func TestRestrictedServerRejectsInvalidInputAndOtherPrincipal(t *testing.T) {
	directory := t.TempDir()
	valid := `{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30}`
	var output bytes.Buffer
	if err := (hostlock.Server{Principal: "planning", Directory: directory}).Serve(t.Context(), strings.NewReader(valid), &output); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"renew", "release", "extend"} {
		output.Reset()
		body := strings.Replace(valid, "acquire", operation, 1)
		if err := (hostlock.Server{Principal: "applying", Directory: directory}).Serve(t.Context(), strings.NewReader(body), &output); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `"status":"not_held"`) {
			t.Fatalf("other principal %s: %s", operation, output.String())
		}
	}
	invalid := []string{
		valid + valid,
		strings.Replace(valid, `"protocol_version":1`, `"protocol_version":1,"protocol_version":1`, 1),
		strings.Replace(valid, `"run_id":"attempt"`, `"run_id":"attempt","directory":"/tmp"`, 1),
		strings.Replace(valid, `"lease_seconds":30`, `"lease_seconds":4201`, 1),
		strings.Replace(valid, `"run_id":"attempt"`, `"run_id":"planning:attempt"`, 1),
		strings.Replace(valid, `"operation":"acquire"`, `"operation":"unlock"`, 1),
		strings.Replace(valid, `"lease_seconds":30`, `"lease_seconds":null`, 1),
		strings.Repeat(" ", 4096) + valid,
	}
	for _, body := range invalid {
		output.Reset()
		if err := (hostlock.Server{Principal: "planning", Directory: directory}).Serve(t.Context(), strings.NewReader(body), &output); err == nil {
			t.Fatal("the server accepted invalid input")
		}
	}
}

func TestRestrictedSetCancelsOnFirstRenewalFailureAndPreservesAbandonedLock(t *testing.T) {
	host, directory := restrictedServer(t, "applying")
	set, err := hostlock.AcquireAllWithOptions(t.Context(), []hostlock.Host{host}, "attempt", "runner",
		hostlock.Lease{TTL: 30 * time.Second, RenewInterval: 100 * time.Millisecond},
		hostlock.Options{Transport: hostlock.RestrictedSSHTransport{}, StrictRenewal: true, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(host.KnownHostsFile); err != nil {
		t.Fatal(err)
	}
	lost := make(chan error, 1)
	stop := set.KeepRenewed(t.Context(), func(err error) { lost <- err })
	defer stop()
	select {
	case err := <-lost:
		if !errors.Is(err, hostlock.ErrLost) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("strict renewal did not cancel work after the first failure")
	}
	if err := scope.Check([]string{host.Name}); !errors.Is(err, hostlock.ErrScope) {
		t.Fatal("lock loss did not invalidate the scope")
	}
	set.Abandon()
	set.ReleaseAll(t.Context())
	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); err != nil {
		t.Fatal("abandon removed the remote lock:", err)
	}
}

func TestRestrictedSetCleansPartialAcquisitionAndReportsReleaseFailure(t *testing.T) {
	host, directory := restrictedServer(t, "applying")
	options := hostlock.Options{Transport: hostlock.RestrictedSSHTransport{}, StrictRenewal: true, RequestTimeout: 5 * time.Second}
	broken := host
	broken.Name = "broken"
	broken.PrivateKeyFile = filepath.Join(t.TempDir(), "missing-key")
	if _, err := hostlock.AcquireAllWithOptions(t.Context(), []hostlock.Host{host, broken}, "partial", "runner", hostlock.DefaultLease(), options); err == nil {
		t.Fatal("acquisition accepted the broken host")
	}
	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial acquisition retained the first lock:", err)
	}
	set, err := hostlock.AcquireAllWithOptions(t.Context(), []hostlock.Host{host}, "attempt", "runner", hostlock.DefaultLease(), options)
	if err != nil {
		t.Fatal(err)
	}
	knownHosts, err := os.ReadFile(host.KnownHostsFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(host.KnownHostsFile); err != nil {
		t.Fatal(err)
	}
	if err := set.ReleaseAllChecked(t.Context()); err == nil {
		t.Fatal("checked release concealed SSH failure")
	}
	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); err != nil {
		t.Fatal("failed release removed the remote lock:", err)
	}
	if err := set.ReleaseAllChecked(t.Context()); err == nil {
		t.Fatal("repeated release falsely reported completion")
	}
	if err := os.WriteFile(host.KnownHostsFile, knownHosts, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := set.ReleaseAllChecked(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release retry retained the remote lock:", err)
	}
}

func TestRestrictedServerPreservesUnsafeFilesystemEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "malformed", "extra-record", "nonempty-guard"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "deploy.lock")
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("invalid\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "extra-record":
				if err := os.WriteFile(path, []byte("old controller 1\nextra\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "nonempty-guard":
				path = filepath.Join(directory, "deploy.lock.guard.old-boot")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "owned-file")
				if err := os.WriteFile(path, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			request := `{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30}`
			if err := (hostlock.Server{Principal: "applying", Directory: directory}).Serve(t.Context(), strings.NewReader(request), &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), `"status":"failed"`) {
				t.Fatal("server accepted unsafe filesystem:", output.String())
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("server removed unsafe filesystem entry:", err)
			}
		})
	}
}

func TestRequestTimeoutStopsBlockedLockCommand(t *testing.T) {
	host := localHost(t)
	holdGuard(t, host)
	started := time.Now()
	_, err := hostlock.AcquireAllWithOptions(t.Context(), []hostlock.Host{host}, "timed-out", "runner", hostlock.DefaultLease(),
		hostlock.Options{RequestTimeout: 100 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request timeout: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("blocked request exceeded its deadline allowance: %s", elapsed)
	}
	if _, err := os.Stat(filepath.Join(host.Dir, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("timed-out request acquired a lock:", err)
	}
}

func TestAbandonInvalidatesEmptyScope(t *testing.T) {
	set, err := hostlock.AcquireAllWithOptions(t.Context(), nil, "empty", "runner", hostlock.DefaultLease(), hostlock.Options{})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Check(nil); err != nil {
		t.Fatal(err)
	}
	set.Abandon()
	if err := scope.Check(nil); !errors.Is(err, hostlock.ErrScope) {
		t.Fatal("abandon did not invalidate the scope")
	}
}

func TestAbandonStopsRenewalAndPreservesRemoteRecord(t *testing.T) {
	host, directory := restrictedServer(t, "applying")
	set, err := hostlock.AcquireAllWithOptions(t.Context(), []hostlock.Host{host}, "abandoned", "runner",
		hostlock.Lease{TTL: 30 * time.Second, RenewInterval: 100 * time.Millisecond},
		hostlock.Options{Transport: hostlock.RestrictedSSHTransport{}, StrictRenewal: true, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deploy.lock")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stop := set.KeepRenewed(t.Context(), func(err error) { t.Errorf("renewal after abandon: %v", err) })
	defer stop()
	set.Abandon()
	time.Sleep(1200 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("abandon changed or removed the remote record: %v", err)
	}
}

func restrictedServer(t *testing.T, principal string) (hostlock.Host, string) {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal("install OpenSSH sshd to run restricted host lock acceptance")
	}
	directory := t.TempDir()
	key := filepath.Join(directory, "client")
	hostKey := filepath.Join(directory, "host")
	for _, path := range []string{key, hostKey} {
		command := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate disposable SSH key: %v: %s", err, output)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	lockDirectory := filepath.Join(directory, "locks")
	config := fmt.Sprintf("Port %s\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s.pub\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nForceCommand %s -test.run=TestRestrictedEndpointProcess -- --host-lock-test %s %s\n", port, hostKey, filepath.Join(directory, "pid"), key, current.Username, executable, principal, lockDirectory)
	configPath := filepath.Join(directory, "sshd_config")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(directory, "sshd.log")
	server := exec.CommandContext(t.Context(), sshd, "-D", "-e", "-f", configPath, "-E", logPath)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		_ = server.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			if err := connection.Close(); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			log, readErr := os.ReadFile(logPath)
			t.Fatalf("start disposable sshd: %v; log read: %v; %s", err, readErr, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	publicKey, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(known, []byte("pinned.example.invalid "+string(publicKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber == 0 {
		t.Fatal("invalid SSH fixture port")
	}
	return hostlock.Host{Name: "restricted", Address: "pinned.example.invalid", ConnectAddress: address, User: current.Username, PrivateKeyFile: key, KnownHostsFile: known}, lockDirectory
}
