package main

import (
	"errors"
	"strconv"
	"strings"

	"goodkind.io/configsctl/internal/statemigration"
)

type migrationArguments struct {
	Action      migrationAction
	Subject     string
	Directory   string
	PlanOptions statemigration.PlanOptions
}

func parseMigrationArguments(arguments []string) (migrationArguments, error) {
	parsed := migrationArguments{Action: "", Subject: "", Directory: "", PlanOptions: statemigration.PlanOptions{Parallelism: 0}}
	positionals := make([]string, 0, 3)
	parallelismSet := false
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		value, found := strings.CutPrefix(argument, "--parallelism=")
		if !found {
			return parsed, errors.New("migration options require --parallelism=N with a positive integer")
		}
		if parallelismSet {
			return parsed, errors.New("specify migration parallelism only once")
		}
		parallelism, err := strconv.Atoi(value)
		if err != nil || parallelism <= 0 || strconv.Itoa(parallelism) != value {
			return parsed, errors.New("migration parallelism requires a positive decimal integer")
		}
		parallelismSet = true
		parsed.PlanOptions.Parallelism = parallelism
	}
	if len(positionals) != 3 {
		return parsed, errors.New(migrationUsage)
	}
	parsed.Action = migrationAction(positionals[0])
	parsed.Subject = positionals[1]
	parsed.Directory = positionals[2]
	switch parsed.Action {
	case migrationPreflight, migrationExecute, migrationResume, migrationVerify, migrationVerifyRestored:
		return parsed, nil
	case migrationInspect, migrationInspectPlan, migrationPrepare, migrationRestore:
		if parallelismSet {
			return parsed, errors.New("--parallelism requires preflight, execute, resume, verify, or verify-restored")
		}
		return parsed, nil
	default:
		return parsed, errors.New(migrationUsage)
	}
}
