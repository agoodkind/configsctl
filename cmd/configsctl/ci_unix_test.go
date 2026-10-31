//go:build unix

package main_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ciWaitingModule configures an external key provider that records the
// OpenTofu process ID and its own process ID, then waits. OpenTofu init runs
// the key provider and waits for its answer.
const ciWaitingModule = `
variable "pid_file" {
  type = string
}

terraform {
  backend "local" {}

  encryption {
    key_provider "external" "wait" {
      command = ["/bin/sh", "-c", "echo \"$PPID $$\" > ${var.pid_file}.tmp; mv ${var.pid_file}.tmp ${var.pid_file}; exec sleep 300"]
    }
    method "aes_gcm" "main" {
      keys = key_provider.external.wait
    }
    state {
      method = method.aes_gcm.main
    }
  }
}
`

const (
	ciPIDName       = "TF_VAR_pid_file"
	ciWaitingConfig = `{"version": 1, "required_environment": ["TF_VAR_pid_file"], "secret_environment": [], "secret_files": {}, "output_mode": "redacted_progress"}`
	ciSignalWait    = 60 * time.Second
)

// TestCITofuStopsOpenTofuOnSignals sends SIGINT and SIGTERM to the built
// configsctl binary while OpenTofu init waits for the key provider. The binary
// must exit with a nonzero status within 60 seconds, and OpenTofu and the key
// provider must stop.
func TestCITofuStopsOpenTofuOnSignals(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(signal.String(), func(t *testing.T) {
			checkout := newCICheckout(t)
			writeFixtureFile(t, filepath.Join(checkout.dir, ciRootName, "main.tf"), ciWaitingModule)
			config := checkout.writeConfig(t, "config.json", ciWaitingConfig)
			pidFile := filepath.Join(checkout.tmpdir, "provider.pid")
			args := []string{
				"ci", "tofu", "--config", config, "--root", ciRootName, "--workspace", "default", "--",
				"init", "-input=false", "-lockfile=readonly", "-no-color",
			}
			command := exec.CommandContext(context.Background(), configsctlBinary, args...)
			command.Dir = checkout.dir
			command.Env = append(checkout.environment(), ciPIDName+"="+pidFile)
			if err := command.Start(); err != nil {
				t.Fatalf("start configsctl ci tofu init: %v", err)
			}
			pids := waitForProviderPIDs(t, pidFile)
			if err := command.Process.Signal(signal); err != nil {
				t.Fatalf("send %s to configsctl: %v", signal, err)
			}
			waited := make(chan error, 1)
			go func() { waited <- command.Wait() }()
			select {
			case err := <-waited:
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
					t.Fatalf("configsctl ci tofu after %s: err = %v, want a nonzero exit", signal, err)
				}
			case <-time.After(ciSignalWait):
				_ = command.Process.Kill()
				t.Fatalf("configsctl ci tofu did not exit within %s after %s", ciSignalWait, signal)
			}
			for _, pid := range pids {
				requireProcessGone(t, pid)
			}
		})
	}
}

// waitForProviderPIDs returns the OpenTofu and key provider process IDs that
// the key provider wrote to pidFile.
func waitForProviderPIDs(t *testing.T, pidFile string) []int {
	t.Helper()
	deadline := time.Now().Add(ciSignalWait)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(pidFile)
		if err == nil {
			fields := strings.Fields(string(body))
			pids := make([]int, 0, len(fields))
			for _, field := range fields {
				pid, err := strconv.Atoi(field)
				if err != nil {
					t.Fatalf("parse the process ID %q in %s: %v", field, pidFile, err)
				}
				pids = append(pids, pid)
			}
			if len(pids) != 2 {
				t.Fatalf("%s contains %q, want two process IDs", pidFile, body)
			}
			t.Cleanup(func() {
				if t.Failed() {
					for _, pid := range pids {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the key provider did not write %s within %s", pidFile, ciSignalWait)
	return nil
}
