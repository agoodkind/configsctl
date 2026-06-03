package lint

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Finding is one input-default or presence violation in a file.
type Finding struct {
	File string
	Line int
	Kind string
	Root string
}

// String renders a finding as a stable line the baseline can key on.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: banned default or presence check: %s on %s",
		f.File, f.Line, f.Kind, f.Root)
}

// Divergence is an expression the gonja parser could not read, or whose operand
// it could not resolve, often a gonja-versus-Jinja precedence split. It is
// listed for review rather than counted as a violation.
type Divergence struct {
	File   string
	Line   int
	Reason string
	Text   string
}

// String renders a divergence as a review line.
func (d Divergence) String() string {
	return fmt.Sprintf("%s:%d: review (%s): %s", d.File, d.Line, d.Reason, d.Text)
}

// Run lints the given files and returns every violation and every divergent
// form, each sorted by file and line. A file path ending in .j2 is read as a
// Jinja template; any other path is read as YAML. An unreadable or unparseable
// file is skipped with a warning.
func Run(paths []string) ([]Finding, []Divergence) {
	var findings []Finding
	var divergences []Divergence
	for _, path := range paths {
		fileFindings, fileDivergences := analyzeFile(path)
		findings = append(findings, fileFindings...)
		divergences = append(divergences, fileDivergences...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	sort.Slice(divergences, func(i, j int) bool {
		if divergences[i].File != divergences[j].File {
			return divergences[i].File < divergences[j].File
		}
		return divergences[i].Line < divergences[j].Line
	})
	return findings, divergences
}

func analyzeFile(path string) ([]Finding, []Divergence) {
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("skip unreadable file", "path", path, "err", err)
		return nil, nil
	}
	exprs, runtime := extractFile(path, string(content))
	var findings []Finding
	var divergences []Divergence
	for _, expr := range exprs {
		constructs, parsed := Analyze("{{ " + expr.text + " }}")
		if !parsed {
			divergences = append(divergences, Divergence{
				File: path, Line: expr.line, Reason: "parse-failed", Text: expr.text,
			})
			continue
		}
		for _, construct := range constructs {
			if construct.Kind != "lookup-default" && construct.Root == "" {
				divergences = append(divergences, Divergence{
					File: path, Line: expr.line, Reason: "unresolved-operand", Text: expr.text,
				})
				continue
			}
			if IsViolation(construct, runtime) {
				findings = append(findings, Finding{
					File: path, Line: expr.line, Kind: construct.Kind, Root: construct.Root,
				})
			}
		}
	}
	return findings, divergences
}

// extractFile returns the expressions and runtime names for a file, choosing
// the Jinja-template reader for .j2 and the YAML reader otherwise.
func extractFile(path, content string) ([]templateExpr, map[string]struct{}) {
	if strings.HasSuffix(path, ".j2") {
		return spanExprs(content, 1), map[string]struct{}{}
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(content), &root); err != nil {
		slog.Warn("skip unparseable yaml", "path", path, "err", err)
		return nil, map[string]struct{}{}
	}
	var exprs []templateExpr
	registers := map[string]struct{}{}
	walkYAML(&root, &exprs, registers)
	return exprs, registers
}
