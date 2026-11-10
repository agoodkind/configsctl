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

	"goodkind.io/configsctl/hostlock"
)

func runHostLockServer(args []string) error {
	if len(args) != 3 || args[0] != "serve" || args[1] != "--principal" || args[2] == "" {
		return errors.New("usage: configsctl host-lock serve --principal ID")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	input, writer := io.Pipe()
	defer input.Close()
	interruptRead := context.AfterFunc(ctx, func() { _ = input.CloseWithError(ctx.Err()) })
	defer interruptRead()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				err := fmt.Errorf("read host lock request panicked: %v", recovered)
				slog.Error("hostlock.input_failed", "err", err)
				_ = writer.CloseWithError(err)
			}
		}()
		_, err := io.Copy(writer, os.Stdin)
		_ = writer.CloseWithError(err)
	}()
	if err := hostlock.Serve(ctx, args[2], input, os.Stdout); err != nil {
		slog.Error("hostlock.server_failed", "err", err)
		return fmt.Errorf("serve host lock request: %w", err)
	}

	return nil
}
