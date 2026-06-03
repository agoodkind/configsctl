package lint

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"goodkind.io/configs/internal/oracle"
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

// Divergence is an expression no parser could read. The Go engine could not
// parse it and the jinja2 oracle could not either, so it is listed for review
// rather than counted as a violation.
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

// pendingForm is an expression the Go engine could not classify, queued for the
// jinja2 oracle. Runtime holds the file's register, set_fact, and loop names so
// the oracle classifies its operand the same way the Go engine would.
type pendingForm struct {
	file    string
	line    int
	reason  string
	text    string
	runtime []string
}

// Run lints the given files and returns every violation and every divergent
// form, each sorted by file and line. A file path ending in .j2 is read as a
// Jinja template; any other path is read as YAML. An unreadable or unparseable
// file is skipped with a warning. Forms the Go engine cannot read are routed to
// the jinja2 oracle; a routing failure is returned, since a routed form cannot
// be silently passed.
func Run(paths []string) ([]Finding, []Divergence, error) {
	var findings []Finding
	var pending []pendingForm
	for _, path := range paths {
		fileFindings, filePending := analyzeFile(path)
		findings = append(findings, fileFindings...)
		pending = append(pending, filePending...)
	}
	routedFindings, divergences, err := routePending(dedupePending(pending))
	if err != nil {
		return nil, nil, err
	}
	findings = dedupeFindings(append(findings, routedFindings...))
	sortFindings(findings)
	sortDivergences(divergences)
	return findings, divergences, nil
}

// routePending sends the queued forms to the jinja2 oracle, turning a resolved
// violation into a finding, dropping a form the oracle read as clean, and
// listing a form the oracle could not read either as a divergence.
func routePending(pending []pendingForm) ([]Finding, []Divergence, error) {
	if len(pending) == 0 {
		return nil, nil, nil
	}
	forms := make([]oracle.Form, len(pending))
	for i, form := range pending {
		forms[i] = oracle.Form{Expr: form.text, Runtime: form.runtime}
	}
	results, err := oracle.Route(forms)
	if err != nil {
		slog.Error("route divergent forms failed", "count", len(forms), "err", err)
		return nil, nil, fmt.Errorf("route divergent forms: %w", err)
	}
	if len(results) != len(pending) {
		return nil, nil, fmt.Errorf("oracle returned %d results for %d forms", len(results), len(pending))
	}
	var findings []Finding
	var divergences []Divergence
	for i, form := range pending {
		result := results[i]
		if !result.Parsed {
			divergences = append(divergences, Divergence{
				File: form.file, Line: form.line, Reason: form.reason, Text: form.text,
			})
			continue
		}
		for _, violation := range result.Violations {
			findings = append(findings, Finding{
				File: form.file, Line: form.line, Kind: violation.Kind, Root: violation.Root,
			})
		}
	}
	return findings, divergences, nil
}

func analyzeFile(path string) ([]Finding, []pendingForm) {
	content, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("skip unreadable file", "path", path, "err", err)
		return nil, nil
	}
	exprs, runtime := extractFile(path, string(content))
	runtimeNames := sortedSet(runtime)
	var findings []Finding
	var pending []pendingForm
	for _, expr := range exprs {
		constructs, parsed := Analyze("{{ " + expr.text + " }}")
		if !parsed {
			pending = append(pending, pendingForm{
				file: path, line: expr.line, reason: "parse-failed",
				text: expr.text, runtime: runtimeNames,
			})
			continue
		}
		for _, construct := range constructs {
			if construct.Kind != "lookup-default" && construct.Root == "" {
				pending = append(pending, pendingForm{
					file: path, line: expr.line, reason: "unresolved-operand",
					text: expr.text, runtime: runtimeNames,
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
	return findings, pending
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

// dedupePending collapses forms that share a file, line, and text, since one
// routing of an expression resolves every construct it carries.
func dedupePending(pending []pendingForm) []pendingForm {
	seen := make(map[string]struct{}, len(pending))
	out := make([]pendingForm, 0, len(pending))
	for _, form := range pending {
		key := fmt.Sprintf("%s:%d:%s", form.file, form.line, form.text)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, form)
	}
	return out
}

// dedupeFindings collapses findings that render to the same line, since the
// baseline keys on that line and a duplicate carries no extra information.
func dedupeFindings(findings []Finding) []Finding {
	seen := make(map[string]struct{}, len(findings))
	out := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := finding.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, finding)
	}
	return out
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}

func sortDivergences(divergences []Divergence) {
	sort.Slice(divergences, func(i, j int) bool {
		if divergences[i].File != divergences[j].File {
			return divergences[i].File < divergences[j].File
		}
		return divergences[i].Line < divergences[j].Line
	})
}

func sortedSet(set map[string]struct{}) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
