// Package tokens is the operator token gate for the input-default linter. The
// gate opens only when the operator supplies a confirm value and a token whose
// slug equals the slug of the gate token command's output. It drives a one-run
// lint bypass and a baseline-write authorization. When the environment variables
// are absent or the token does not match, the gate stays shut.
package tokens

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tokenCommandTimeout bounds the operator token command, which typically fetches
// a value over the network.
const tokenCommandTimeout = 15 * time.Second

// DefaultGateTokenCmd fetches today's Wikipedia featured-article canonical title.
// Both sides run through Slugify, so the raw title is compared as a slug.
const DefaultGateTokenCmd = `curl -fsSL ` +
	`"https://en.wikipedia.org/api/rest_v1/feed/featured/$(date -u +%Y/%m/%d)" ` +
	`| jq -r ".tfa.titles.canonical"`

const bypassConfirmValue = "1"

var affirmativeConfirmValues = map[string]struct{}{
	"1": {}, "y": {}, "yes": {}, "Y": {}, "YES": {},
}

// Slugify lowercases the text and keeps only a-z, 0-9, underscore, and hyphen.
func Slugify(text string) string {
	var builder strings.Builder
	for _, char := range text {
		switch {
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char - 'A' + 'a')
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_' || char == '-':
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func confirmAccepted(value string) bool {
	_, ok := affirmativeConfirmValues[value]
	return ok
}

// runTokenCommand runs the operator-supplied token command through sh -c and
// returns its stdout and whether it exited zero. A nonzero exit shuts the gate.
func runTokenCommand(command string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
	if err != nil {
		slog.Warn("token command failed", "err", err)
		return "", false
	}
	return string(out), true
}

func tokensMatch(expectedRaw, actualRaw string) bool {
	expected := Slugify(expectedRaw)
	actual := Slugify(actualRaw)
	if expected == "" || actual == "" {
		return false
	}
	return expected == actual
}

// BypassPasses reports whether one lint run should be non-blocking, and the
// matched token. It opens only when BYPASS_LINT slugs non-empty, BYPASS_CONFIRM
// is exactly "1", and the BYPASS_LINT slug equals the token command's slug.
func BypassPasses() (bool, string) {
	bypass := Slugify(os.Getenv("BYPASS_LINT"))
	if bypass == "" {
		return false, ""
	}
	if os.Getenv("BYPASS_CONFIRM") != bypassConfirmValue {
		return false, ""
	}
	command := envOr("BYPASS_TOKEN_CMD", DefaultGateTokenCmd)
	expectedRaw, ok := runTokenCommand(command)
	if !ok {
		return false, ""
	}
	expected := Slugify(expectedRaw)
	if expected == "" || bypass != expected {
		return false, ""
	}
	return true, expected
}

// BaselineGatePasses reports whether a baseline refresh is authorized. It opens
// only when BASELINE_CONFIRM is affirmative and the token command's slug equals
// the BASELINE_TOKEN slug.
func BaselineGatePasses() bool {
	if !confirmAccepted(os.Getenv("BASELINE_CONFIRM")) {
		return false
	}
	command := envOr("BASELINE_TOKEN_CMD", DefaultGateTokenCmd)
	expectedRaw, ok := runTokenCommand(command)
	if !ok {
		return false
	}
	return tokensMatch(expectedRaw, os.Getenv("BASELINE_TOKEN"))
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
