package retirement

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

var errUsage = errors.New("usage: configsctl ci retirement --base <sha> --head <sha>")

// Command checks retired addresses from the current configuration checkout.
//
// The arguments select the trusted base and proposed head commits.
//
// Example:
//
//	configsctl ci retirement --base <sha> --head <sha>
func Command(args []string) error {
	if len(args) != 4 || args[0] != "--base" || args[2] != "--head" || args[3] == "" {
		return errUsage
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return Verify(ctx, ".", args[1], args[3])
}
