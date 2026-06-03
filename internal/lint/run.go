package lint

import (
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

// Run lints the given files and returns every violation, sorted by file and
// line. A file path ending in .j2 is read as a Jinja template; any other path
// is read as YAML. An unreadable or unparseable file is skipped with a warning.
func Run(paths []string) []Finding {
	var findings []Finding
	for _, path := range paths {
		findings = append(findings, analyzeFile(path)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings
}

func analyzeFile(path string) []Finding {
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("skip unreadable file", "path", path, "err", err)
		return nil
	}
	exprs, runtime := extractFile(path, string(content))
	var findings []Finding
	for _, expr := range exprs {
		for _, construct := range FindConstructs("{{ " + expr.text + " }}") {
			if IsViolation(construct, runtime) {
				findings = append(findings, Finding{
					File: path, Line: expr.line, Kind: construct.Kind, Root: construct.Root,
				})
			}
		}
	}
	return findings
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
