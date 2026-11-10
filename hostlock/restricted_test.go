package hostlock_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

func TestMain(tests *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "--host-lock-") {
		if err := runRestrictedEndpoint(os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	os.Exit(tests.Run())
}

// The SSH subprocess uses the production endpoint with an isolated lock directory.
func runRestrictedEndpoint(args []string) error {
	switch args[0] {
	case "--host-lock-reply":
		if len(args) != 3 {
			return errors.New("reply subprocess requires a response file and lock directory")
		}

		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			return err
		}

		body, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}

		if _, err := os.Stderr.Write(bytes.Repeat([]byte("x"), 65536)); err != nil {
			return err
		}

		_, err = os.Stdout.Write(body)

		return err
	case "--host-lock-test":
		if len(args) != 3 && len(args) != 4 {
			return errors.New(
				"server subprocess requires a principal, optional reply marker, and lock directory",
			)
		}

		var output io.Writer = os.Stdout
		if len(args) == 4 {
			_, err := os.Stat(args[2])
			switch {
			case err == nil:
				output = io.Discard
			case !errors.Is(err, os.ErrNotExist):
				return err
			}
		}

		return (hostlock.Server{Principal: args[1], Directory: args[len(args)-1]}).Serve(
			context.Background(),
			os.Stdin,
			output,
		)
	default:
		return errors.New("unknown host lock subprocess operation")
	}
}

func TestRestrictedSSHUsesPinnedIdentityAndSharedLocks(t *testing.T) {
	host, directory := restrictedServer(t, "planning")
	transport := hostlock.RestrictedSSHTransport{}
	request := hostlock.Request{
		Operation:  hostlock.Acquire,
		Run:        "attempt-one",
		Controller: "runner",
		Lease:      30 * time.Second,
	}
	if _, err := transport.Do(t.Context(), host, request); err != nil {
		t.Fatal(err)
	}

	legacy := hostlock.Host{Name: "legacy", Dir: directory}
	if err := hostlock.Do(
		t.Context(),
		legacy,
		hostlock.Acquire,
		"legacy-run",
		"local",
	); err == nil {
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

	if err := hostlock.Do(
		t.Context(),
		legacy,
		hostlock.Acquire,
		"legacy-run",
		"local",
	); err != nil {
		t.Fatal(err)
	}

	request.Operation = hostlock.Acquire
	if _, err := transport.Do(t.Context(), host, request); err == nil {
		t.Fatal("the restricted client acquired the legacy client's lock")
	}

	if err := hostlock.Do(
		t.Context(),
		legacy,
		hostlock.Release,
		"legacy-run",
		"local",
	); err != nil {
		t.Fatal(err)
	}

	wrongIdentity := host
	wrongIdentity.Address = "untrusted.example.invalid"
	if _, err := transport.Do(t.Context(), wrongIdentity, request); err == nil {
		t.Fatal("SSH accepted an unpinned endpoint identity")
	}

	if _, err := transport.Do(t.Context(), host, request); err != nil {
		t.Fatalf("the pinned identity could not acquire the free lock: %v", err)
	}

	wrongKey := host
	wrongKey.PrivateKeyFile = filepath.Join(t.TempDir(), "missing-key")
	if _, err := transport.Do(t.Context(), wrongKey, request); err == nil {
		t.Fatal("SSH accepted a missing client key")
	}
}

func TestRestrictedServerRejectsInvalidInputAndOtherPrincipal(t *testing.T) {
	directory := physicalTempDir(t)
	valid := `{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30}`
	var output bytes.Buffer
	if err := (hostlock.Server{Principal: "planning", Directory: directory}).Serve(
		t.Context(),
		strings.NewReader(valid),
		&output,
	); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"renew", "release", "extend"} {
		output.Reset()
		body := strings.Replace(valid, "acquire", operation, 1)
		if err := (hostlock.Server{Principal: "applying", Directory: directory}).Serve(
			t.Context(),
			strings.NewReader(body),
			&output,
		); err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(output.String(), `"status":"not_held"`) {
			t.Fatalf("other principal %s: %s", operation, output.String())
		}
	}

	invalid := []string{
		valid + valid,
		strings.Replace(
			valid,
			`"operation":"acquire"`,
			`"operation":"acquire","OPERATION":"release"`,
			1,
		),
		strings.Replace(valid, `"run_id"`, `"Run_Id"`, 1),
		strings.Replace(valid, `,"lease_seconds":30`, "", 1),
		strings.Replace(
			valid,
			`"protocol_version":1`,
			`"protocol_version":1,"protocol_version":1`,
			1,
		),
		strings.Replace(valid, `"run_id":"attempt"`, `"run_id":"attempt","directory":"/tmp"`, 1),
		strings.Replace(valid, `"lease_seconds":30`, `"lease_seconds":4201`, 1),
		strings.Replace(valid, `"run_id":"attempt"`, `"run_id":"planning:attempt"`, 1),
		strings.Replace(valid, `"operation":"acquire"`, `"operation":"unlock"`, 1),
		strings.Replace(valid, `"lease_seconds":30`, `"lease_seconds":null`, 1),
		strings.Repeat(" ", 4096) + valid,
	}
	for _, body := range invalid {
		output.Reset()
		if err := (hostlock.Server{Principal: "planning", Directory: directory}).Serve(
			t.Context(),
			strings.NewReader(body),
			&output,
		); err == nil {
			t.Fatal("the server accepted invalid input")
		}
	}
}

func TestRestrictedSetCancelsOnFirstRenewalFailureAndPreservesAbandonedLock(t *testing.T) {
	host, directory := restrictedServer(t, "applying")
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"attempt",
		"runner",
		hostlock.Lease{TTL: 30 * time.Second, RenewInterval: 100 * time.Millisecond},
		hostlock.Options{
			Transport:      hostlock.RestrictedSSHTransport{},
			StrictRenewal:  true,
			RequestTimeout: 5 * time.Second,
		},
	)
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
	options := hostlock.Options{
		Transport:      hostlock.RestrictedSSHTransport{},
		StrictRenewal:  true,
		RequestTimeout: 5 * time.Second,
	}
	broken := host
	broken.Name = "broken"
	broken.PrivateKeyFile = filepath.Join(t.TempDir(), "missing-key")
	if _, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host, broken},
		"partial",
		"runner",
		hostlock.DefaultLease(),
		options,
	); err == nil {
		t.Fatal("acquisition accepted the broken host")
	}

	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial acquisition retained the first lock:", err)
	}

	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"attempt",
		"runner",
		hostlock.DefaultLease(),
		options,
	)
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
	for _, kind := range []string{
		"symlink",
		"malformed",
		"extra-record",
		"nonempty-guard",
		"leading-zero-epoch",
	} {
		t.Run(
			kind,
			func(t *testing.T) {
				directory := physicalTempDir(t)
				path := filepath.Join(directory, "deploy.lock")
				switch kind {
				case "leading-zero-epoch":
					if err := os.WriteFile(
						path,
						[]byte("prior owner 02000000009\n"),
						0o600,
					); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Symlink(filepath.Join(t.TempDir(), "target"), path); err != nil {
						t.Fatal(err)
					}
				case "malformed":
					if err := os.WriteFile(path, []byte("invalid\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "extra-record":
					if err := os.WriteFile(
						path,
						[]byte("old controller 1\nextra\n"),
						0o600,
					); err != nil {
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
				if err := (hostlock.Server{Principal: "applying", Directory: directory}).Serve(
					t.Context(),
					strings.NewReader(request),
					&output,
				); err != nil {
					t.Fatal(err)
				}

				if !strings.Contains(output.String(), `"status":"failed"`) {
					t.Fatal("server accepted unsafe filesystem:", output.String())
				}

				if _, err := os.Lstat(path); err != nil {
					t.Fatal("server removed unsafe filesystem entry:", err)
				}

				if kind == "leading-zero-epoch" {
					body, err := os.ReadFile(path)
					if err != nil || string(body) != "prior owner 02000000009\n" {
						t.Fatalf(
							"conflicting acquire changed the existing record: %q, %v",
							body,
							err,
						)
					}
				}
			},
		)
	}
}

func TestRequestTimeoutStopsBlockedLockCommand(t *testing.T) {
	host := localHost(t)
	holdGuard(t, host)
	started := time.Now()
	_, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"timed-out",
		"runner",
		hostlock.DefaultLease(),
		hostlock.Options{RequestTimeout: 100 * time.Millisecond},
	)
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

func TestRenewalTimeoutUsesConfiguredOrLegacyDeadline(t *testing.T) {
	for _, timeout := range []time.Duration{0, 100 * time.Millisecond} {
		t.Run(
			timeout.String(),
			func(t *testing.T) {
				host := localHost(t)
				lease := hostlock.Lease{TTL: 30 * time.Second, RenewInterval: 2 * time.Second}
				options := hostlock.Options{StrictRenewal: true, RequestTimeout: timeout}
				set, err := hostlock.AcquireAllWithOptions(
					t.Context(),
					[]hostlock.Host{host},
					"attempt",
					"runner",
					lease,
					options,
				)
				if err != nil {
					t.Fatal(err)
				}
				defer set.Abandon()
				holdGuard(t, host)
				started := time.Now()
				err = set.RenewAll(t.Context())
				if !errors.Is(err, hostlock.ErrLost) {
					t.Fatalf("blocked strict renewal did not report loss: %v", err)
				}

				deadline := timeout
				if timeout == 0 {
					deadline = lease.RenewTimeout()
					if !strings.Contains(
						err.Error(),
						"the host did not answer within "+deadline.String(),
					) {
						t.Fatalf("legacy renewal lost its timeout cause: %v", err)
					}
				} else if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("configured renewal did not report its request deadline: %v", err)
				}

				if elapsed := time.Since(started); elapsed > deadline+attemptSlack {
					t.Fatalf("renewal took %s with a %s deadline", elapsed, deadline)
				}
			},
		)
	}
}

func TestAbandonInvalidatesEmptyScope(t *testing.T) {
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		nil,
		"empty",
		"runner",
		hostlock.DefaultLease(),
		hostlock.Options{},
	)
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
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"abandoned",
		"runner",
		hostlock.Lease{TTL: 30 * time.Second, RenewInterval: 100 * time.Millisecond},
		hostlock.Options{
			Transport:      hostlock.RestrictedSSHTransport{},
			StrictRenewal:  true,
			RequestTimeout: 5 * time.Second,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "deploy.lock")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	stop := set.KeepRenewed(
		t.Context(),
		func(err error) { t.Errorf("renewal after abandon: %v", err) },
	)
	defer stop()
	set.Abandon()
	time.Sleep(1200 * time.Millisecond)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("abandon changed or removed the remote record: %v", err)
	}
}

func TestRestrictedServerRefusesSymlinkedAncestor(t *testing.T) {
	root, target := physicalTempDir(t), physicalTempDir(t)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}

	server := hostlock.Server{
		Principal: "planning",
		Directory: filepath.Join(alias, "missing", "locks"),
	}
	body := `{"protocol_version":1,"operation":"acquire","run_id":"attempt","lease_seconds":30}`
	var output bytes.Buffer
	if err := server.Serve(t.Context(), strings.NewReader(body), &output); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output.String(), `"status":"failed"`) {
		t.Fatal("the server followed a symlinked ancestor:", output.String())
	}

	if _, err := os.Stat(filepath.Join(target, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the server created a directory through a symlink: %v", err)
	}
}

func TestRestrictedExtendUsesSavedEndpointAndTimeout(t *testing.T) {
	host, directory := restrictedServer(t, "planning")
	options := hostlock.Options{
		Transport:      &hostlock.RestrictedSSHTransport{},
		RequestTimeout: 4 * time.Second,
	}
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		nil,
		"attempt",
		"runner",
		hostlock.DefaultLease(),
		options,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer set.ReleaseAll(t.Context())
	if err := set.AcquireAdditional(t.Context(), []hostlock.Host{host}); err != nil {
		t.Fatal(err)
	}

	expiry, err := hostlock.Extend(t.Context(), set.StateDir(), host.Name, hostlock.MaxExtend)
	if err != nil {
		t.Fatal(err)
	}

	if stored := lockExpiry(
		t,
		filepath.Join(directory, "deploy.lock"),
		"planning:attempt",
	); !stored.Equal(expiry) {
		t.Fatalf("the server stored expiry %s but Extend reported %s", stored, expiry)
	}

	t.Run(
		"saved deadline",
		func(t *testing.T) {
			holdGuard(t, hostlock.Host{Name: host.Name, Dir: directory})
			_, err := hostlock.Extend(t.Context(), set.StateDir(), host.Name, hostlock.MaxExtend)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the blocked extension did not report its saved timeout: %v", err)
			}
		},
	)
}

func TestExtendRefusesAnUnsavedCustomTransport(t *testing.T) {
	host, _ := restrictedServer(t, "planning")
	options := hostlock.Options{
		Transport: struct {
			hostlock.RestrictedSSHTransport
		}{},
	}
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"attempt",
		"runner",
		hostlock.DefaultLease(),
		options,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer set.ReleaseAll(t.Context())
	if _, err := hostlock.Extend(
		t.Context(),
		set.StateDir(),
		host.Name,
		hostlock.MaxExtend,
	); err == nil ||
		!strings.Contains(err.Error(), "child processes cannot extend") {
		t.Fatalf("child extension did not refuse the custom transport: %v", err)
	}
}

func TestCheckedReleaseRetriesAfterServerDeletesLock(t *testing.T) {
	marker := filepath.Join(physicalTempDir(t), "discard-reply")
	host, directory := restrictedSSHServer(t, "--host-lock-test applying "+marker)
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"attempt",
		"runner",
		hostlock.DefaultLease(),
		hostlock.Options{Transport: hostlock.RestrictedSSHTransport{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Abandon()
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := set.ReleaseAllChecked(t.Context()); err == nil {
		t.Fatal("checked release accepted an empty SSH reply")
	}

	if _, err := os.Stat(filepath.Join(directory, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the server did not delete the lock before losing the reply: %v", err)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	other := hostlock.Host{Name: "legacy", Dir: directory}
	if err := hostlock.Do(t.Context(), other, hostlock.Acquire, "other-run", "local"); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(directory, "deploy.lock")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := set.ReleaseAllChecked(t.Context()); !errors.Is(err, hostlock.ErrNotHeld) {
		t.Fatalf("retry did not report the replacement owner: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("retry changed the replacement owner's record: %q, %v", after, err)
	}

	if err := hostlock.Do(t.Context(), other, hostlock.Release, "other-run", "local"); err != nil {
		t.Fatal(err)
	}

	if err := set.ReleaseAllChecked(t.Context()); err != nil {
		t.Fatalf("the server refused a retry after deleting the lock: %v", err)
	}
}

func TestManualStrictRenewalCancelsSupervisedWork(t *testing.T) {
	host := localHost(t)
	set, err := hostlock.AcquireAllWithOptions(
		t.Context(),
		[]hostlock.Host{host},
		"attempt",
		"runner",
		hostlock.DefaultLease(),
		hostlock.Options{StrictRenewal: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Abandon()
	scope, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}

	work, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	stop := set.KeepRenewed(t.Context(), cancel)
	defer stop()
	if err := hostlock.Do(t.Context(), host, hostlock.Release, "attempt", "runner"); err != nil {
		t.Fatal(err)
	}

	if err := set.RenewAll(t.Context()); !errors.Is(err, hostlock.ErrLost) {
		t.Fatalf("manual renewal did not report loss: %v", err)
	}

	select {
	case <-work.Done():
		if !errors.Is(context.Cause(work), hostlock.ErrLost) {
			t.Fatal(context.Cause(work))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the supervisor continued after manual renewal failed")
	}

	if err := scope.Check([]string{host.Name}); !errors.Is(err, hostlock.ErrScope) {
		t.Fatalf("scope accepted work after manual renewal failed: %v", err)
	}
}

func TestRestrictedSSHValidatesBoundedReplies(t *testing.T) {
	reply := filepath.Join(physicalTempDir(t), "reply")
	host, _ := restrictedSSHServer(t, "--host-lock-reply "+reply)

	const valid = `{"status":"ok","host_time":1000,"expires_at":1030}`
	for _, test := range []struct {
		name  string
		op    hostlock.Operation
		reply string
		ok    bool
	}{
		{"complete acquire", hostlock.Acquire, valid, true},
		{"missing host time", hostlock.Acquire, `{"status":"ok","expires_at":1030}`, false},
		{"missing release expiry", hostlock.Release, `{"status":"ok","host_time":1000}`, false},
		{"alias", hostlock.Acquire, strings.Replace(valid, "host_time", "HOST_TIME", 1), false},
		{"excess output", hostlock.Acquire, valid + strings.Repeat(" ", 4096), false},
		{"invalid clock", hostlock.Acquire, `{"status":"ok","host_time":0,"expires_at":30}`, false},
		{"long acquire", hostlock.Acquire, `{"status":"ok","host_time":1,"expires_at":1030}`, false},
		{
			"expired renewal",
			hostlock.Renew,
			`{"status":"ok","host_time":1000,"expires_at":999}`,
			false,
		},
		{
			"excess renewal",
			hostlock.Renew,
			`{"status":"ok","host_time":1000,"expires_at":9999999999}`,
			false,
		},
		{
			"preserved expiry",
			hostlock.Operation("extend"),
			`{"status":"ok","host_time":1000,"expires_at":5200}`,
			true,
		},
		{"release with lease", hostlock.Release, valid, false},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				if err := os.WriteFile(reply, []byte(test.reply), 0o600); err != nil {
					t.Fatal(err)
				}

				_, err := (hostlock.RestrictedSSHTransport{}).Do(
					t.Context(),
					host,
					hostlock.Request{Operation: test.op, Run: "attempt", Lease: 30 * time.Second},
				)
				if (err == nil) != test.ok {
					t.Fatalf("reply acceptance differed from %t: %v", test.ok, err)
				}
			},
		)
	}
}

func physicalTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return directory
}

func restrictedServer(t *testing.T, principal string) (hostlock.Host, string) {
	t.Helper()
	return restrictedSSHServer(t, "--host-lock-test "+principal)
}

func restrictedSSHServer(t *testing.T, endpointArgs string) (hostlock.Host, string) {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal("install OpenSSH sshd to run restricted host lock acceptance")
	}

	directory := physicalTempDir(t)
	key := filepath.Join(directory, "client")

	hostKey := filepath.Join(directory, "host")
	for _, path := range []string{key, hostKey} {
		command := exec.CommandContext(
			t.Context(),
			"ssh-keygen",
			"-q",
			"-t",
			"ed25519",
			"-N",
			"",
			"-f",
			path,
		)
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
	config := fmt.Sprintf(
		"Port %s\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s.pub\nStrictModes no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nAllowUsers %s\nForceCommand %s %s %s\n",
		port,
		hostKey,
		filepath.Join(directory, "pid"),
		key,
		current.Username,
		executable,
		endpointArgs,
		lockDirectory,
	)
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
	if err := os.WriteFile(
		known,
		[]byte("pinned.example.invalid "+string(publicKey)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber == 0 {
		t.Fatal("invalid SSH fixture port")
	}

	return hostlock.Host{
		Name:           "restricted",
		Address:        "pinned.example.invalid",
		ConnectAddress: address,
		User:           current.Username,
		PrivateKeyFile: key,
		KnownHostsFile: known,
	}, lockDirectory
}
