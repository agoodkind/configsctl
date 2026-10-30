package tofurun

import (
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/runlog"
	"goodkind.io/configsctl/internal/tofuargs"
)

// openOutput creates and announces a run log with the request's redaction patterns.
func openOutput(request Request, display []string) (*runlog.Log, error) {
	log, err := runlog.Open("tofu-"+string(tofuargs.Subcommand(display)), request.Patterns)
	if err != nil {
		slog.Error("tofu.log_open_failed", "workspace", request.Root, "err", err)
		return nil, fmt.Errorf("open the OpenTofu run log: %w", err)
	}
	log.Announce("Tofu")
	return log, nil
}

// closeOutput closes the run log and reports a close failure.
func closeOutput(log *runlog.Log) error {
	if err := log.Close(); err != nil {
		slog.Error("tofu.log_close_failed", "log", log.Path(), "err", err)
		return fmt.Errorf("close the OpenTofu run log: %w", err)
	}
	return nil
}
