package hostlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

// StateEnvVar is the environment key for the state directory of the locks of
// a running deploy. A child process passes its value to Extend.
const StateEnvVar = "CONFIGS_LOCK_STATE"

const (
	stateFile       = "run.json"
	declarationFile = "lease-%d.json"
	stateFileMode   = 0o600
)

type lockedHost struct {
	Name    string `json:"name"`
	User    string `json:"user"`
	Address string `json:"address"`
	Dir     string `json:"dir"`
}

// runState is the run and the hosts that a Set locked.
type runState struct {
	Run        string       `json:"run"`
	Controller string       `json:"controller"`
	Hosts      []lockedHost `json:"hosts"`
}

// declaration is the lease that Extend wrote on one host. Asked is the
// controller time before the script call.
type declaration struct {
	Asked            time.Time `json:"asked"`
	RemainingSeconds int64     `json:"remaining_seconds"`
}

func newStateDir() (string, error) {
	dir, err := os.MkdirTemp("", "configsctl-lock-*")
	if err != nil {
		slog.Error("hostlock.state_dir_failed", "err", err)
		return "", fmt.Errorf("create the lock state directory: %w", err)
	}
	return dir, nil
}

func removeStateDir(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("hostlock.state_dir_remove_failed", "dir", dir, "err", err)
	}
}

func writeState(dir, run, controller string, hosts []Host) error {
	state := runState{Run: run, Controller: controller, Hosts: make([]lockedHost, 0, len(hosts))}
	for _, host := range hosts {
		state.Hosts = append(state.Hosts, lockedHost(host))
	}
	return writeJSONFile(filepath.Join(dir, stateFile), state)
}

func writeJSONFile[T runState | declaration](path string, value T) error {
	body, err := json.Marshal(value)
	if err != nil {
		slog.Error("hostlock.state_encode_failed", "path", path, "err", err)
		return fmt.Errorf("encode %s: %w", path, err)
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, body, stateFileMode); err != nil {
		slog.Error("hostlock.state_write_failed", "path", path, "err", err)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		slog.Error("hostlock.state_write_failed", "path", path, "err", err)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func declarationPath(dir string, index int) string {
	return filepath.Join(dir, fmt.Sprintf(declarationFile, index))
}

func readState(dir string) (runState, error) {
	var state runState
	path := filepath.Join(dir, stateFile)
	body, err := os.ReadFile(path)
	if err != nil {
		slog.Error("hostlock.state_unreadable", "path", path, "err", err)
		return state, fmt.Errorf("read the locks of the running deploy: %w", err)
	}
	if err := json.Unmarshal(body, &state); err != nil {
		slog.Error("hostlock.state_unreadable", "path", path, "err", err)
		return state, fmt.Errorf("decode the locks of the running deploy in %s: %w", path, err)
	}
	return state, nil
}

// Extend declares a lease of duration only for hosts locked by the
// running deploy recorded in stateDir.
// The host lock script returns the held error to other runs until the
// declared expiry.
// Set reads the declaration before reporting a lost lock.
func Extend(ctx context.Context, stateDir, hostName string, duration time.Duration) (time.Time, error) {
	state, err := readState(stateDir)
	if err != nil {
		return time.Time{}, err
	}
	index := slices.IndexFunc(state.Hosts, func(locked lockedHost) bool { return locked.Name == hostName })
	if index < 0 {
		err = fmt.Errorf("run %s has no lock on host %s", state.Run, hostName)
		slog.Error("hostlock.extend.host_not_locked", "host", hostName, "run", state.Run, "err", err)
		return time.Time{}, err
	}
	asked := clock.NowUTC()
	request := command{op: extend, run: state.Run, controller: state.Controller, lease: duration}
	extended, err := runScript(ctx, Host(state.Hosts[index]), request)
	if err != nil {
		slog.Error("hostlock.extend.failed", "host", hostName, "run", state.Run, "err", err)
		return time.Time{}, err
	}
	written := declaration{Asked: asked, RemainingSeconds: int64(extended.remaining / time.Second)}
	if err := writeJSONFile(declarationPath(stateDir, index), written); err != nil {
		return time.Time{}, err
	}
	slog.Info("hostlock.extended", "host", hostName, "run", state.Run,
		"expiry", extended.expiry.UTC().Format(time.RFC3339), "seconds", written.RemainingSeconds)
	return extended.expiry, nil
}

// readDeclarations sets each host's loss deadline one RenewInterval before
// the expiry declared by Extend.
func (s *Set) readDeclarations() {
	s.mu.Lock()
	hosts := append([]Host{}, s.held...)
	s.mu.Unlock()
	for index, host := range hosts {
		path := declarationPath(s.stateDir, index)
		body, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		var declared declaration
		if err == nil {
			err = json.Unmarshal(body, &declared)
		}
		if err != nil {
			slog.Warn("hostlock.declaration_unreadable", "path", path, "host", host.Name, "err", err)
			continue
		}
		s.advance(host.Name, declared.Asked, time.Duration(declared.RemainingSeconds)*time.Second)
	}
}
