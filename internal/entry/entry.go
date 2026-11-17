// Package entry implements formatting before vault access.
package entry

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
)

var errFormatUsage = errors.New("usage: configsctl fmt")

// Format rejects command arguments before loading native settings.
//
// loadDirectory reads the native settings. execute runs OpenTofu with the
// supplied environment and arguments.
//
// Pass an empty args slice to format the configured directory:
//
//	Format(nil, loadDirectory, execute)
func Format(
	args []string,
	loadDirectory func() (string, error),
	execute func(context.Context, string, []string, []string) error,
) error {
	if len(args) != 0 {
		return errFormatUsage
	}

	directory, err := loadDirectory()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	arguments := []string{"fmt", "-recursive"}
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}

	return execute(ctx, directory, arguments, environment)
}
