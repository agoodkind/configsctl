package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"goodkind.io/configsctl/hostlock"
)

func runHostLockServer(args []string) error {
	if len(args) != 3 || args[0] != "serve" || args[1] != "--principal" || args[2] == "" {
		return errors.New("usage: configsctl host-lock serve --principal ID")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := hostlock.Serve(ctx, args[2], os.Stdin, os.Stdout); err != nil {
		slog.Error("hostlock.server_failed", "err", err)
		return fmt.Errorf("serve host lock request: %w", err)
	}
	return nil
}
