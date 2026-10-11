package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
)

const lockUsage = "usage: configsctl lock extend --host <inventory host> --seconds <seconds>"

// runLock extends a lease only for a host locked by the running deploy.
// A play can run configsctl lock extend on the controller before restarting
// the host.
func runLock(_ cmdEnv, args []string) error {
	if len(args) == 0 || args[0] != "extend" {
		return errors.New(lockUsage)
	}
	host, seconds, err := parseLockExtend(args[1:])
	if err != nil {
		return err
	}
	stateDir := os.Getenv(hostlock.StateEnvVar)
	if stateDir == "" {
		return fmt.Errorf("%s is empty: run lock extend from a task of a running deploy", hostlock.StateEnvVar)
	}
	expiry, err := hostlock.Extend(context.Background(), stateDir, host, time.Duration(seconds)*time.Second)
	if err != nil {
		slog.Error("lock.extend_failed", "host", host, "seconds", seconds, "err", err)
		return fmt.Errorf("extend the lock on %s: %w", host, err)
	}
	fmt.Printf("The lock on %s lasts until %s.\n", host, expiry.UTC().Format(time.RFC3339))
	return nil
}

type lockExtendFlag string

const (
	lockHostFlag    lockExtendFlag = "--host"
	lockSecondsFlag lockExtendFlag = "--seconds"
)

func parseLockExtend(args []string) (host string, seconds int64, err error) {
	secondsText := ""
	for index := 0; index < len(args); index++ {
		name, value, glued := strings.Cut(args[index], "=")
		if !glued && index+1 < len(args) {
			index++
			value = args[index]
		}
		switch lockExtendFlag(name) {
		case lockHostFlag:
			host = value
		case lockSecondsFlag:
			secondsText = value
		default:
			return "", 0, fmt.Errorf("unknown lock extend argument: %q", name)
		}
	}
	if host == "" || secondsText == "" {
		return "", 0, errors.New(lockUsage)
	}
	seconds, err = strconv.ParseInt(secondsText, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("--seconds %q is not a whole number of seconds", secondsText)
	}
	return host, seconds, nil
}
