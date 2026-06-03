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

// Gate lints the paths, drops findings already in the baseline, and returns the
// new findings, whether the run should block, and a bypass token when a matching
// operator token made the run non-blocking.
func Gate(paths []string) ([]string, bool, string) {
	current := findingStrings(Run(paths))
	keys := baseline.Load(readBaselineLines(), baseline.Label).Keys()
	newFindings, _ := baseline.Evaluate(current, keys)
	if len(newFindings) == 0 {
		return nil, false, ""
	}
	if passed, token := tokens.BypassPasses(); passed {
		return newFindings, false, token
	}
	return newFindings, true, ""
}

// WriteBaseline records the current findings as accepted when the operator token
// gate is open, and reports whether it wrote. A write failure is logged.
func WriteBaseline(paths []string, mode baseline.Mode) bool {
	if !tokens.BaselineGatePasses() {
		return false
	}
	current := findingStrings(Run(paths))
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

func readBaselineLines() []string {
	data, err := os.ReadFile(BaselineFile)
	if err != nil {
		return nil
	}
	return strings.Split(string(data), "\n")
}
