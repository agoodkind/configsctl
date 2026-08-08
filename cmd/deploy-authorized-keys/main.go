// Command deploy-authorized-keys writes SSH authorization bundles from GitHub keys.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/configs/internal/authorizedkeys"
)

func main() {
	slog.Debug("deploy authorized keys command started")
	err := authorizedkeys.Run(context.Background(), os.Args[1:], os.Stdout, nil)
	if errors.Is(err, authorizedkeys.ErrHelp) {
		return
	}
	if err != nil {
		if !authorizedkeys.IsLogged(err) {
			fmt.Fprintf(os.Stderr, "deploy-authorized-keys: %v\n", err)
		}
		os.Exit(1)
	}
}
