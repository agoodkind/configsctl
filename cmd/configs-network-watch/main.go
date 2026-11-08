//go:build linux

// Command configs-network-watch monitors a route or interface.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

const (
	exitFailure   = 1
	exitUsage     = 2
	exitInterrupt = 130
)

const usageText = `usage:
  configs-network-watch route --destination PREFIX --gateway ADDRESS --interface NAME
      --interval-seconds SECONDS --failure-threshold COUNT --metric METRIC
  configs-network-watch interface --interface NAME --dhcp4-unit UNIT
      --dhcp6-unit UNIT --route-unit UNIT
`

// subcommand is the first command-line argument.
type subcommand string

const (
	routeSubcommand     subcommand = "route"
	interfaceSubcommand subcommand = "interface"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	status := run(os.Args[1:], os.Stderr)
	slog.Info("network_watch.exit", "status", status)
	os.Exit(status)
}

// errInterrupted is the cancel cause after SIGINT.
var errInterrupted = errors.New("interrupted by SIGINT")

// errTerminated is the cancel cause after SIGTERM.
var errTerminated = errors.New("terminated by SIGTERM")

// run parses the subcommand, runs it until it fails or a stop signal arrives,
// and returns the process exit status.
func run(args []string, stderr io.Writer) int {
	command, err := parseCommand(args)
	if err != nil {
		fmt.Fprintf(stderr, "configs-network-watch: %v\n%s", err, usageText)
		return exitUsage
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr := fmt.Errorf("signal handler panic: %v", recovered)
				slog.Error("network_watch.signal_handler_panic", "err", panicErr)
				cancel(panicErr)
			}
		}()
		select {
		case received := <-signals:
			slog.Info("network_watch.stop_signal", "signal", received.String())
			if received == syscall.SIGINT {
				cancel(errInterrupted)
			} else {
				cancel(errTerminated)
			}
		case <-ctx.Done():
		}
	}()

	if err := command(ctx); err != nil {
		slog.Error("network_watch.failed", "err", err)
		return exitFailure
	}
	if errors.Is(context.Cause(ctx), errInterrupted) {
		return exitInterrupt
	}
	return 0
}

// commandFunc runs one parsed subcommand until ctx ends or it fails.
type commandFunc func(ctx context.Context) error

func parseCommand(args []string) (commandFunc, error) {
	if len(args) == 0 {
		return nil, usageError("missing subcommand")
	}
	switch subcommand(args[0]) {
	case routeSubcommand:
		config, err := parseRouteConfig(args[1:])
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error {
			return runRouteTracker(ctx, config)
		}, nil
	case interfaceSubcommand:
		config, err := parseInterfaceConfig(args[1:])
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) error {
			return runInterfaceWatcher(ctx, config)
		}, nil
	default:
		return nil, usageError(fmt.Sprintf("unknown subcommand %q", args[0]))
	}
}
