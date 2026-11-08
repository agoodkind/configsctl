//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	systemddbus "github.com/coreos/go-systemd/v22/dbus"
)

const (
	jobModeReplace     = "replace"
	jobResultDone      = "done"
	loadStateMissing   = "not-found"
	jobConnectionCheck = time.Second
)

// errJobConnectionClosed marks a job wait that ended because the systemd
// connection closed before systemd reported the job result.
var errJobConnectionClosed = errors.New("systemd connection closed before the job result")

// activeStates lists the ActiveState values that `systemctl is-active`
// accepts as active.
var activeStates = []string{"active", "reloading", "refreshing"}

// restartVerb selects the systemd restart job.
type restartVerb string

const (
	restartIfRunning restartVerb = "try-restart"
	restartAlways    restartVerb = "restart"
)

// unitFailure logs a failed systemd operation and returns it as an error.
func unitFailure(unit string, operation string, err error) error {
	wrapped := fmt.Errorf("%s %s: %w", operation, unit, err)
	slog.Warn("network_watch.systemd.operation_failed", "unit", unit, "operation", operation, "err", err)
	return wrapped
}

// unitManager queries and restarts systemd units over the systemd private
// socket. It opens a new connection after the previous one closes.
type unitManager struct {
	connection *systemddbus.Conn
}

func (manager *unitManager) connect(ctx context.Context, unit string) (*systemddbus.Conn, error) {
	if manager.connection != nil && manager.connection.Connected() {
		return manager.connection, nil
	}
	manager.close()
	connection, err := systemddbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return nil, unitFailure(unit, "connect to systemd for", err)
	}
	manager.connection = connection
	return connection, nil
}

func (manager *unitManager) close() {
	if manager.connection != nil {
		manager.connection.Close()
		manager.connection = nil
	}
}

func (manager *unitManager) unitProperty(ctx context.Context, unit string, name string) (string, error) {
	connection, err := manager.connect(ctx, unit)
	if err != nil {
		return "", err
	}
	operation := "read " + name + " of"
	property, err := connection.GetUnitPropertyContext(ctx, unit, name)
	if err != nil {
		return "", unitFailure(unit, operation, err)
	}
	value, ok := property.Value.Value().(string)
	if !ok {
		return "", unitFailure(unit, operation, fmt.Errorf("value %s is not a string", property.Value.String()))
	}
	return value, nil
}

func (manager *unitManager) installed(ctx context.Context, unit string) (bool, error) {
	loadState, err := manager.unitProperty(ctx, unit, "LoadState")
	if err != nil {
		return false, err
	}
	return loadState != loadStateMissing, nil
}

func (manager *unitManager) active(ctx context.Context, unit string) (bool, error) {
	activeState, err := manager.unitProperty(ctx, unit, "ActiveState")
	if err != nil {
		return false, err
	}
	return slices.Contains(activeStates, activeState), nil
}

// restart runs verb for an installed unit and waits for the job result.
// It skips a unit that systemd cannot find.
func (manager *unitManager) restart(ctx context.Context, unit string, verb restartVerb) error {
	installed, err := manager.installed(ctx, unit)
	if err != nil {
		return err
	}
	if !installed {
		return nil
	}
	connection, err := manager.connect(ctx, unit)
	if err != nil {
		return err
	}
	operation := string(verb)
	results := make(chan string, 1)
	switch verb {
	case restartIfRunning:
		_, err = connection.TryRestartUnitContext(ctx, unit, jobModeReplace, results)
	case restartAlways:
		_, err = connection.RestartUnitContext(ctx, unit, jobModeReplace, results)
	default:
		return unitFailure(unit, operation, errors.New("unknown restart verb"))
	}
	if err != nil {
		return unitFailure(unit, operation, err)
	}
	return awaitJob(ctx, connection, unit, operation, results)
}

// awaitJob waits for the result of one systemd job. It fails when ctx ends
// or when the connection closes before systemd reports the result.
func awaitJob(
	ctx context.Context, connection *systemddbus.Conn, unit string, operation string, results <-chan string,
) error {
	ticker := time.NewTicker(jobConnectionCheck)
	defer ticker.Stop()
	for {
		select {
		case result := <-results:
			return jobOutcome(unit, operation, result)
		case <-ctx.Done():
			return unitFailure(unit, operation, ctx.Err())
		case <-ticker.C:
			if connection.Connected() {
				continue
			}
			select {
			case result := <-results:
				return jobOutcome(unit, operation, result)
			default:
				return unitFailure(unit, operation, errJobConnectionClosed)
			}
		}
	}
}

// jobOutcome converts a systemd job result into an error, or logs the
// restart and returns nil for the result done.
func jobOutcome(unit string, operation string, result string) error {
	if result != jobResultDone {
		return unitFailure(unit, operation, fmt.Errorf("job result %s", result))
	}
	slog.Info("network_watch.systemd.restarted", "unit", unit, "verb", operation)
	return nil
}
