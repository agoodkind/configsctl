package retirement

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

var errUsage = errors.New(
	"usage: configsctl ci retirement prepare|verify|publish or configsctl ci retirement --base <sha> --head <sha>",
)

// Command checks retired addresses from the current configuration checkout.
//
// The arguments select immutable commits or a GitHub workflow mode.
// Prepare fetches event commits, verify checks retirement, and publish records
// a status.
//
// Example:
//
//	BASE_SHA=881ea0946db0f2a9990b35836fed00a00a5f355d
//	HEAD_SHA=97d4b0f5c84f74157814b3d7b88b4d92cb001ac1
//	configsctl ci retirement --base "$BASE_SHA" --head "$HEAD_SHA"
func Command(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(args) == 1 && (args[0] == "prepare" || args[0] == "verify" || args[0] == "publish") {
		return githubCommand(ctx, args[0])
	}

	if len(args) != 4 || args[0] != "--base" || args[2] != "--head" || args[3] == "" {
		return errUsage
	}

	return Verify(ctx, ".", args[1], args[3])
}
