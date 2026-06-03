package lint

import (
	"log/slog"
	"os"
	"strings"

	"goodkind.io/configs/internal/baseline"
	"goodkind.io/configs/internal/clock"
	"goodkind.io/configs/internal/tokens"
)

// BaselineFile is the accepted-findings record, read and written at the working
// directory root.
const BaselineFile = ".ansible-defaults-baseline.txt"

// Result is the outcome of a gated lint run.
type Result struct {
	NewFindings []string
	Divergences []string
	Blocked     bool
	BypassToken string
}

// Gate lints the paths, drops findings already in the baseline, and returns the
// new findings, the divergent forms to review, whether the run should block, and
// a bypass token when a matching operator token made the run non-blocking.
func Gate(paths []string) Result {
	findings, divergences := Run(paths)
	current := findingStrings(findings)
	divergenceLines := divergenceStrings(divergences)
	keys := baseline.Load(readBaselineLines(), baseline.Label).Keys()
	newFindings, _ := baseline.Evaluate(current, keys)
	if len(newFindings) == 0 {
		return Result{NewFindings: nil, Divergences: divergenceLines, Blocked: false, BypassToken: ""}
	}
	if passed, token := tokens.BypassPasses(); passed {
		return Result{NewFindings: newFindings, Divergences: divergenceLines, Blocked: false, BypassToken: token}
	}
	return Result{NewFindings: newFindings, Divergences: divergenceLines, Blocked: true, BypassToken: ""}
}

// WriteBaseline records the current findings as accepted when the operator token
// gate is open, and reports whether it wrote. A write failure is logged.
func WriteBaseline(paths []string, mode baseline.Mode) bool {
	if !tokens.BaselineGatePasses() {
		return false
	}
	findings, _ := Run(paths)
	current := findingStrings(findings)
	now := clock.Stamp()
	body := baseline.RewriteBody(current, readBaselineLines(), baseline.Label, now, mode)
	content := baseline.Render(baseline.Label, now, body)
	if err := os.WriteFile(BaselineFile, []byte(content), 0o600); err != nil {
		slog.Error("write baseline failed", "path", BaselineFile, "err", err)
		return false
	}
	return true
}

func findingStrings(findings []Finding) []string {
	lines := make([]string, len(findings))
	for i, finding := range findings {
		lines[i] = finding.String()
	}
	return lines
}

func divergenceStrings(divergences []Divergence) []string {
	lines := make([]string, len(divergences))
	for i, divergence := range divergences {
		lines[i] = divergence.String()
	}
	return lines
}

func readBaselineLines() []string {
	data, err := os.ReadFile(BaselineFile)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}
