//go:build unix

package migrationpause_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/migrationpause"
)

const (
	leaseModeVariable = "CONFIGSCTL_LEASE_TEST_MODE"
	leasePathVariable = "CONFIGSCTL_LEASE_TEST_PATH"
	leaseProcessWait  = 15 * time.Second
)

func TestLeaseProcess(t *testing.T) {
	mode := os.Getenv(leaseModeVariable)
	if mode == "" {
		t.Skip("the parent test selects a lease subprocess mode")
	}
	pause := os.Getenv(leasePathVariable)
	if pause == "" {
		t.Fatal("the lease subprocess requires its private pause path")
	}
	acquire := migrationpause.AcquireWriter
	if mode == "exclusive" || mode == "paused-exit" {
		acquire = migrationpause.AcquireExclusive
	}
	release, err := acquire(pause)
	if err != nil {
		fmt.Fprintln(os.Stdout, "refused: "+err.Error())
		return
	}
	defer release()
	if mode == "paused-exit" {
		if err := os.WriteFile(pause, []byte("synthetic-retained-pause"), 0o600); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "paused")
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatalf("the lease subprocess did not receive its completion signal: %v", err)
	}
}

func leaseCommand(t *testing.T, mode, pause string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseProcessWait)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestLeaseProcess$")
	command.Env = append(os.Environ(), leaseModeVariable+"="+mode, leasePathVariable+"="+pause)
	return command, cancel
}

func holdLeaseProcess(t *testing.T, mode, pause string) func() {
	t.Helper()
	command, cancel := leaseCommand(t, mode, pause)
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_, writeErr := io.WriteString(input, "done\n")
			_ = input.Close()
			waitErr := command.Wait()
			cancel()
			if writeErr != nil || waitErr != nil {
				t.Errorf("the real lease subprocess failed: write=%v wait=%v stderr=%s", writeErr, waitErr, stderr.String())
			}
		})
	}
	t.Cleanup(stop)
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("the real lease subprocess did not acquire its lease: %q, %v", line, err)
	}
	return stop
}

func requireLeaseRefusal(t *testing.T, mode, pause, reason string) {
	t.Helper()
	command, cancel := leaseCommand(t, mode, pause)
	defer cancel()
	command.Stdin = strings.NewReader("done\n")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the lease probe failed instead of reporting a refusal: %v, %s", err, output)
	}
	if !strings.Contains(string(output), "refused: "+reason) {
		t.Fatalf("the real lease probe did not report %q: %s", reason, output)
	}
}

func TestOrdinaryWritersCoexistAndExcludeMigration(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "controller", "pause.json")
	first := holdLeaseProcess(t, "writer", pause)
	second := holdLeaseProcess(t, "writer", pause)
	requireLeaseRefusal(t, "exclusive", pause, "another infrastructure writer owns")
	first()
	requireLeaseRefusal(t, "exclusive", pause, "another infrastructure writer owns")
	second()
	exclusive := holdLeaseProcess(t, "exclusive", pause)
	exclusive()
}

func TestMigrationExcludesOrdinaryWriters(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "controller", "pause.json")
	exclusive := holdLeaseProcess(t, "exclusive", pause)
	requireLeaseRefusal(t, "writer", pause, "another infrastructure writer owns")
	exclusive()
	writer := holdLeaseProcess(t, "writer", pause)
	writer()
}

func TestPauseSurvivesExecutorExitAndRejectsWriter(t *testing.T) {
	pause := filepath.Join(t.TempDir(), "controller", "pause.json")
	command, cancel := leaseCommand(t, "paused-exit", pause)
	defer cancel()
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "paused\n") {
		t.Fatalf("the executor did not publish its retained pause: %v, %s", err, output)
	}
	if err := migrationpause.Guard(pause); err == nil {
		t.Fatal("executor exit removed the infrastructure write pause")
	}
	requireLeaseRefusal(t, "writer", pause, "infrastructure writes remain paused")
}
