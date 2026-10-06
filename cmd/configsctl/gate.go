package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configsctl/internal/gate"
)

const (
	controllerConfigsDir = "/srv/configs"
	controllerRunsDir    = "/var/lib/configs-runs"
)

// runGate is the forced command of a requester key on a deploy controller.
func runGate(_ cmdEnv, args []string) error {
	if len(args) != 2 || args[0] != "--requester" {
		return errors.New("usage: configsctl gate --requester <name>")
	}
	self, err := os.Executable()
	if err != nil {
		slog.Error("gate.self_unavailable", "err", err)
		return fmt.Errorf("resolve the configsctl binary path: %w", err)
	}
	controller, err := os.Hostname()
	if err != nil {
		slog.Error("gate.hostname_unavailable", "err", err)
		return fmt.Errorf("read the controller host name: %w", err)
	}
	passwordFile, err := vaultPassPath()
	if err != nil {
		slog.Error("gate.vault_password_path_unavailable", "err", err)
		return fmt.Errorf("resolve the vault password file: %w", err)
	}
	server := gate.Server{
		Repo:              gate.Repo{Dir: controllerConfigsDir},
		Runs:              gate.Runs{Dir: controllerRunsDir},
		Requester:         args[1],
		Controller:        controller,
		Self:              self,
		Out:               os.Stdout,
		VaultPasswordFile: passwordFile,
	}
	if err := server.Serve(context.Background(), os.Getenv("SSH_ORIGINAL_COMMAND"), os.Stdin); err != nil {
		slog.Error("gate.request.refused", "requester", args[1], "err", err)
		return fmt.Errorf("gate: %w", err)
	}
	return nil
}

// runGateFinish records the end of a run. systemd runs it as ExecStopPost of
// the run unit.
func runGateFinish(_ cmdEnv, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: configsctl gate-finish <run id>")
	}
	runs := gate.Runs{Dir: controllerRunsDir}
	if err := runs.Finish(context.Background(), gate.Repo{Dir: controllerConfigsDir}, args[0]); err != nil {
		slog.Error("gate.finish.failed", "run", args[0], "err", err)
		return fmt.Errorf("finish run %s: %w", args[0], err)
	}
	return nil
}
