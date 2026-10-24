//go:build unix

package statemigration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/migrationpause"
	"goodkind.io/configsctl/internal/statemigration"
)

type sessionProbeMode string

const (
	sessionWriterProbe         sessionProbeMode = "writer"
	sessionExclusiveProbe      sessionProbeMode = "exclusive"
	sessionExecutorProbe       sessionProbeMode = "executor"
	sessionEnvironmentProbe    sessionProbeMode = "environment"
	sessionProbeModeKey                         = "CONFIGSCTL_SESSION_PROBE_MODE"
	sessionProbePathKey                         = "CONFIGSCTL_SESSION_PROBE_PATH"
	sessionRawEnvironmentKey                    = "CONFIGSCTL_SESSION_RAW_ENVIRONMENT"
	sessionRawEnvironmentValue                  = "synthetic-invalid-\xff-byte"
)

func TestMigrationSessionLeaseProcess(t *testing.T) {
	mode := sessionProbeMode(os.Getenv(sessionProbeModeKey))
	if mode == "" {
		return
	}
	path := os.Getenv(sessionProbePathKey)
	if path == "" {
		t.Fatal("the session lease probe requires its private path")
	}
	var release func()
	var err error
	switch mode {
	case sessionWriterProbe:
		release, err = migrationpause.AcquireWriter(path)
	case sessionExclusiveProbe:
		release, err = migrationpause.AcquireExclusive(path)
	case sessionExecutorProbe:
		file, openErr := os.OpenFile(filepath.Join(path, "executor.lock"), os.O_RDWR, 0o600)
		if openErr != nil {
			t.Fatal(openErr)
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		release = func() { _ = file.Close() }
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			release()
			t.Fatal(err)
		}
	case sessionEnvironmentProbe:
		if os.Getenv(sessionRawEnvironmentKey) != sessionRawEnvironmentValue {
			t.Fatal("the actual child environment changed the invalid UTF-8 bytes")
		}
		fmt.Fprintln(os.Stdout, "environment-preserved")
		return
	default:
		t.Fatal("the session lease probe requires a supported operation")
	}
	if release != nil {
		defer release()
	}
	if err != nil {
		fmt.Fprintln(os.Stdout, "refused: "+err.Error())
		return
	}
	fmt.Fprintln(os.Stdout, "acquired")
}

func requireSessionLeaseProbe(t *testing.T, mode sessionProbeMode, path, expected string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestMigrationSessionLeaseProcess$")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), sessionProbeModeKey + "=" + string(mode), sessionProbePathKey + "=" + path,
	}
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), expected) {
		t.Fatalf("the real session lease probe did not report %q: %v, %s", expected, err, output)
	}
}

func TestMigrationSessionExecutesAndVerifiesUnderOneLease(t *testing.T) {
	engine, before := migrationFixture(t)
	var captured statemigration.Engine
	err := engine.WithSession(t.Context(), func(owned statemigration.Engine) error {
		captured = owned
		requireSessionLeaseProbe(t, sessionWriterProbe, owned.PauseFile, "refused: an active migration")
		requireSessionLeaseProbe(t, sessionExecutorProbe, owned.Directory, "refused:")
		if err := owned.Prepare(t.Context()); err != nil {
			return err
		}
		if err := owned.Preflight(t.Context(), owned.Manifest.ConfigurationDirectory, "workspaces"); err != nil {
			return err
		}
		if err := owned.Execute(t.Context()); err != nil {
			return err
		}
		if err := owned.Verify(t.Context()); err != nil {
			return err
		}
		requireSessionLeaseProbe(t, sessionWriterProbe, owned.PauseFile, "refused: an active migration")
		requireSessionLeaseProbe(t, sessionExecutorProbe, owned.Directory, "refused:")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := captured.Verify(t.Context()); err == nil || !strings.Contains(err.Error(), "session is closed") {
		t.Fatalf("the captured engine reused closed session ownership: %v", err)
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
	requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
	after := restoredState(t, engine, "application")
	if after.Records["terraform_data.renamed"] != before.Records["terraform_data.original"] || len(restoredState(t, engine, "legacy").Records) != 0 {
		t.Fatal("the migration session changed complete provider records or retained duplicate ownership")
	}
	requirePrivateRecoveryFiles(t, engine)
}

func TestMigrationSessionReleasesLocksAfterCallbackFailureAndCancellation(t *testing.T) {
	engine, _ := migrationFixture(t)
	before := readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))
	stopped := errors.New("the synthetic migration callback stopped")
	err := engine.WithSession(t.Context(), func(owned statemigration.Engine) error {
		requireSessionLeaseProbe(t, sessionWriterProbe, owned.PauseFile, "refused: an active migration")
		return stopped
	})
	if !errors.Is(err, stopped) {
		t.Fatalf("the migration session changed its callback failure: %v", err)
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
	requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = engine.WithSession(ctx, func(owned statemigration.Engine) error {
		cancel()
		if err := owned.Prepare(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("the captured engine ignored session cancellation: %v", err)
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("the migration session reported success after cancellation")
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
	requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
	if !bytes.Equal(before, readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))) {
		t.Fatal("the failed or canceled callback changed the encrypted backend")
	}
	if _, err := os.Stat(filepath.Join(engine.Directory, "journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the failed or canceled callback wrote a migration journal")
	}
}

func TestMigrationSessionRefusesChangedImmutableOwnership(t *testing.T) {
	for _, mutation := range []struct {
		name   string
		change func(*statemigration.Engine)
	}{
		{"manifest", func(engine *statemigration.Engine) { engine.Manifest.RestoreCommit = strings.Repeat("d", 40) }},
		{"directory", func(engine *statemigration.Engine) { engine.Directory = filepath.Join(engine.Directory, "changed") }},
		{"pause", func(engine *statemigration.Engine) { engine.PauseFile += ".changed" }},
		{"parallelism", func(engine *statemigration.Engine) { engine.PlanOptions.Parallelism = 1 }},
		{"workspace", func(engine *statemigration.Engine) {
			workspace := engine.Workspaces["legacy"]
			workspace.Directory = engine.OriginalWorkspaces["legacy"].Directory
			engine.Workspaces["legacy"] = workspace
		}},
		{"environment", func(engine *statemigration.Engine) {
			workspace := engine.Workspaces["legacy"]
			workspace.Environment = append(workspace.Environment, "TF_HTTP_ADDRESS=http://127.0.0.1:9/state")
			engine.Workspaces["legacy"] = workspace
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			engine, _ := migrationFixture(t)
			before := readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))
			err := engine.WithSession(t.Context(), func(owned statemigration.Engine) error {
				mutation.change(&owned)
				return owned.Prepare(t.Context())
			})
			if err == nil || !strings.Contains(err.Error(), "immutable ownership") {
				t.Fatalf("the captured engine accepted changed %s ownership: %v", mutation.name, err)
			}
			if !bytes.Equal(before, readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))) {
				t.Fatal("a changed session engine altered the original encrypted backend")
			}
			requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
			requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
		})
	}
}

func TestMigrationSessionPreservesExactChildEnvironmentBytes(t *testing.T) {
	engine, _ := migrationFixture(t)
	workspace := engine.Workspaces["legacy"]
	workspace.Environment = append(workspace.Environment, sessionRawEnvironmentKey+"="+sessionRawEnvironmentValue)
	engine.Workspaces["legacy"] = workspace
	before := readNativeTestBytes(t, fixtureBackendPath(t, workspace))
	err := engine.WithSession(t.Context(), func(owned statemigration.Engine) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		selected := owned.Workspaces["legacy"]
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestMigrationSessionLeaseProcess$")
		command.Env = append(selected.Environment, sessionProbeModeKey+"="+string(sessionEnvironmentProbe), sessionProbePathKey+"="+owned.Directory)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "environment-preserved") {
			t.Fatalf("the real child did not preserve the session environment bytes: %v", err)
		}
		selected.Environment = append(selected.Environment, sessionRawEnvironmentKey+"=synthetic-invalid-\xfe-byte")
		owned.Workspaces["legacy"] = selected
		if err := owned.Prepare(t.Context()); err == nil || !strings.Contains(err.Error(), "immutable ownership") {
			t.Fatalf("the session accepted different invalid UTF-8 environment bytes: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, readNativeTestBytes(t, fixtureBackendPath(t, workspace))) {
		t.Fatal("the environment-byte refusal changed the native backend")
	}
}

func TestMigrationSessionObservesCancellationWhileDrainingNativeWork(t *testing.T) {
	engine, _ := migrationFixture(t)
	before := readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	operationContext, stopOperation := context.WithTimeout(t.Context(), time.Minute)
	defer stopOperation()
	returning := make(chan statemigration.Engine, 1)
	operationResult := make(chan error, 1)
	sessionResult := make(chan error, 1)
	finished := false
	defer func() {
		cancel()
		stopOperation()
		if !finished {
			select {
			case <-sessionResult:
			case <-time.After(time.Minute):
				t.Error("the native migration session did not finish cleanup")
			}
		}
	}()
	go func() {
		sessionResult <- engine.WithSession(ctx, func(owned statemigration.Engine) error {
			go func() { operationResult <- owned.Prepare(operationContext) }()
			if err := waitForSessionPreparation(owned.Directory); err != nil {
				return err
			}
			returning <- owned
			return nil
		})
	}()
	var captured statemigration.Engine
	select {
	case captured = <-returning:
	case err := <-sessionResult:
		finished = true
		t.Fatalf("the native session stopped before its callback returned: %v", err)
	}
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		err := captured.Verify(t.Context())
		if err != nil && strings.Contains(err.Error(), "session is closed") {
			break
		}
		if err == nil || !strings.Contains(err.Error(), "another operation") {
			t.Fatalf("the concurrent public operation did not respect session ownership: %v", err)
		}
		select {
		case <-timeout.C:
			t.Fatal("the migration session did not begin draining its native operation")
		case <-ticker.C:
		}
	}
	select {
	case err := <-operationResult:
		t.Fatalf("the native operation finished before cancellation during the drain: %v", err)
	default:
	}
	cancel()
	if err := <-operationResult; err != nil {
		t.Fatalf("the independent native preparation did not finish: %v", err)
	}
	err := <-sessionResult
	finished = true
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("session closure reported success after cancellation during its drain: %v", err)
	}
	if !bytes.Equal(before, readNativeTestBytes(t, fixtureBackendPath(t, engine.Workspaces["legacy"]))) {
		t.Fatal("the drained preparation changed the actual encrypted backend")
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
	requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
	requirePrivateRecoveryFiles(t, engine)
}

func waitForSessionPreparation(directory string) error {
	timeout := time.NewTimer(time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		paths, err := filepath.Glob(filepath.Join(directory, "prepare-*"))
		if err != nil {
			return fmt.Errorf("inspect the native preparation directory: %w", err)
		}
		if len(paths) != 0 {
			return nil
		}
		select {
		case <-timeout.C:
			return errors.New("the real native preparation did not create its private working directory")
		case <-ticker.C:
		}
	}
}

func TestMigrationSessionRetainsPauseAfterInterruptedNativeTransfer(t *testing.T) {
	engine, before := migrationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := make(chan error, 1)
	started := false
	err := engine.WithSession(ctx, func(owned statemigration.Engine) error {
		if err := owned.Prepare(ctx); err != nil {
			return err
		}
		done := make(chan struct{})
		defer close(done)
		started = true
		go func() { observed <- cancelAfterSessionWrite(owned.Directory, done, cancel) }()
		return owned.Execute(ctx)
	})
	if !started {
		t.Fatalf("the native session stopped before its transfer: %v", err)
	}
	if observedErr := <-observed; observedErr != nil {
		t.Fatal(observedErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the interrupted native session did not preserve cancellation: %v", err)
	}
	if len(restoredState(t, engine, "legacy").Records) != 0 {
		t.Fatal("the interruption did not occur after a real source backend write")
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "refused: infrastructure writes remain paused")
	requireSessionLeaseProbe(t, sessionExclusiveProbe, engine.PauseFile, "acquired")
	requireSessionLeaseProbe(t, sessionExecutorProbe, engine.Directory, "acquired")
	requirePrivateRecoveryFiles(t, engine)
	err = engine.WithSession(t.Context(), func(owned statemigration.Engine) error {
		if err := owned.Execute(t.Context()); err != nil {
			return err
		}
		return owned.Verify(t.Context())
	})
	if err != nil {
		t.Fatal(err)
	}
	if restoredState(t, engine, "application").Records["terraform_data.renamed"] != before.Records["terraform_data.original"] {
		t.Fatal("session recovery changed the transferred provider record")
	}
	requireSessionLeaseProbe(t, sessionWriterProbe, engine.PauseFile, "acquired")
	requirePrivateRecoveryFiles(t, engine)
}

func cancelAfterSessionWrite(directory string, done <-chan struct{}, cancel context.CancelFunc) error {
	timeout := time.NewTimer(2 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(directory, "journal.json"))
		if err != nil {
			cancel()
			return fmt.Errorf("read the real migration checkpoint: %w", err)
		}
		var checkpoint struct {
			Phase   string   `json:"phase"`
			Written []string `json:"written"`
		}
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			cancel()
			return errors.New("the actual migration checkpoint did not decode")
		}
		if checkpoint.Phase == "applying" && len(checkpoint.Written) != 0 {
			cancel()
			return nil
		}
		select {
		case <-done:
			return errors.New("the native transfer ended before its first confirmed write")
		case <-timeout.C:
			cancel()
			return errors.New("the native transfer did not confirm a write before the observation timeout")
		case <-ticker.C:
		}
	}
}
