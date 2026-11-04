package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"goodkind.io/configsctl/internal/tofuargs"
)

var nativeApplyFlags = map[string]bool{"backup": true, "state-out": true, "suppress-forget-errors": true}

var nativeSharedFlags = map[string]bool{
	"lock": true, "lock-timeout": true, "input": true, "parallelism": true,
	"state": true, "no-color": true, "compact-warnings": true, "concise": true,
	"consolidate-warnings": true, "consolidate-errors": true, "show-sensitive": true,
	"json": true, "json-into": true, "deprecation": true,
	"lint": true,
}

func nativeMutationArguments(args []string, planPath string) ([]string, []string, error) {
	plan := []string{string(tofuargs.Plan)}
	apply := []string{string(tofuargs.Apply)}
	if tofuargs.Subcommand(args) == tofuargs.Destroy {
		plan = append(plan, "-destroy")
	}
	parallelism := false
	saved := savedPlanCandidate(args) != ""
	for index := 1; index < len(args); index++ {
		argument := args[index]
		if !strings.HasPrefix(argument, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(argument, "-"), "=")
		if name == "auto-approve" {
			if _, value, found := strings.Cut(argument, "="); found {
				if _, err := strconv.ParseBool(value); err != nil {
					slog.Error("tofu.mutation.approval_flag_invalid", "err", err)
					return nil, nil, fmt.Errorf("invalid -auto-approve value: %w", err)
				}
			}
			continue
		}
		if tofuargs.ConsumesValue(argument) {
			index++
			if index >= len(args) {
				return nil, nil, fmt.Errorf("OpenTofu option %s requires a value", argument)
			}
			argument += "=" + args[index]
		}
		parallelism = parallelism || name == "parallelism"
		switch {
		case name == "var" || name == "var-file":
			plan = append(plan, argument)
			if saved {
				apply = append(apply, argument)
			}
		case nativeApplyFlags[name]:
			apply = append(apply, argument)
		case nativeSharedFlags[name]:
			plan = append(plan, argument)
			apply = append(apply, argument)
		default:
			plan = append(plan, argument)
		}
	}
	if !parallelism {
		value := "-parallelism=" + strconv.Itoa(explicitDefaultParallelism)
		plan = append(plan, value)
		apply = append(apply, value)
	}
	plan = append(plan, "-input=false", "-no-color", "-detailed-exitcode", "-out="+planPath)
	apply = append(apply, "-input=false", "-no-color", planPath)
	return plan, apply, nil
}

func copyPrivateFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		slog.Error("tofu.lock.plan_read_failed", "path", source, "err", err)
		return fmt.Errorf("read the saved plan %s: %w", source, err)
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		slog.Error("tofu.lock.plan_copy_failed", "path", destination, "err", err)
		return fmt.Errorf("create the saved plan copy %s: %w", destination, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		slog.Error("tofu.lock.plan_copy_failed", "path", destination, "err", err)
		return fmt.Errorf("write the saved plan copy %s: %w", destination, err)
	}
	if err := file.Close(); err != nil {
		slog.Error("tofu.lock.plan_copy_failed", "path", destination, "err", err)
		return fmt.Errorf("close the saved plan copy %s: %w", destination, err)
	}
	return nil
}
