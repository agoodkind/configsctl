package gate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/vault"
)

var errNoSecrets = errors.New("the vault has no secret values")

// The vault lookup uses the controller checkout because sshd starts the forced
// command in root's home directory.
func (s Server) secretPatterns() ([]redact.Pattern, error) {
	patterns, err := vault.Patterns(s.Repo.Dir, s.VaultPasswordFile)
	if err != nil {
		slog.Error("gate.secrets.load_failed", "root", s.Repo.Dir, "err", err)
		return nil, fmt.Errorf("load the vault under %s: %w", s.Repo.Dir, err)
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("vault under %s: %w", s.Repo.Dir, errNoSecrets)
	}
	if badKey, ok := redact.Validate(patterns); !ok {
		return nil, fmt.Errorf("vault key %q has a value shorter than %d characters", badKey, redact.MinLen)
	}
	return patterns, nil
}

func (s Server) withheld(req Request, cause error) outcome {
	stored := req
	stored.ExtraVars = nil
	slog.Error("gate.secrets.unavailable", "requester", s.Requester, "kind", string(req.Kind), "err", cause)
	return outcome{stored: stored, run: "", err: fmt.Errorf("refuse the request without vault secret patterns: %w", cause)}
}

func (s Server) answer(ctx context.Context, req, stored Request, secrets []redact.Pattern) outcome {
	out := redact.New(s.Out, secrets)
	masked := s
	masked.Out = out
	run, err := masked.dispatch(ctx, req, stored)
	if closeErr := out.Close(); closeErr != nil {
		slog.Error("gate.answer.flush_failed", "run", run, "err", closeErr)
		err = errors.Join(err, fmt.Errorf("flush the answer: %w", closeErr))
	}
	return outcome{stored: stored, run: run, err: err}
}
