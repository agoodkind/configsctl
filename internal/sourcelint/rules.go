package sourcelint

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var (
	errSortedKeys    = errors.New("remove redundant sort around keys in for expressions")
	errInvalidSource = errors.New("invalid OpenTofu source")
)

const (
	sourceLabelName  = "name"
	sourceTypeName   = "type"
	provisionerBlock = "provisioner"
	moduleBlock      = "module"
	variableBlock    = "variable"
)

type sourceFinding struct {
	location hcl.Range
	rule     string
	message  string
	blocking bool
}

type sourceRules struct {
	literals         map[string]map[string][]hcl.Range
	findings         []sourceFinding
	scripts          map[string]bool
	originalLocation *hcl.Range
}

func checkSourceRules(directory, root string) error {
	rules := sourceRules{
		literals:         make(map[string]map[string][]hcl.Range),
		findings:         nil,
		scripts:          make(map[string]bool),
		originalLocation: nil,
	}
	input, err := os.OpenRoot(directory)
	if err != nil {
		slog.Error("tofu.source_lint.rules_open_failed", "err", err)

		return fmt.Errorf("open source rules input: %w", err)
	}
	defer input.Close()

	err = fs.WalkDir(
		input.FS(),
		".",
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if entry.IsDir() ||
				(!strings.HasSuffix(path, ".tf") && !strings.HasSuffix(path, ".tf.json")) {
				return nil
			}

			original, err := originalFilename(filepath.Join(directory, path), directory, root)
			if err != nil {
				return err
			}

			contents, err := input.ReadFile(path)
			if err != nil {
				slog.Error("tofu.source_lint.rules_read_failed", "err", err)

				return fmt.Errorf("read source rule input: %w", err)
			}

			return rules.readSource(contents, original)
		},
	)
	if err != nil {
		slog.Error("tofu.source_lint.rules_failed", "err", err)
	}

	rules.repeatedLiterals()

	return errors.Join(err, rules.print())
}

func (rules *sourceRules) readSource(contents []byte, filename string) error {
	if strings.HasSuffix(filename, ".json") {
		return rules.readJSON(contents, filename)
	}

	file, diagnostics := hclsyntax.ParseConfig(contents, filename, hcl.InitialPos)
	if diagnostics.HasErrors() {
		return sourceParseError(filename, diagnostics)
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil
	}

	return rules.body(body, filepath.Dir(filename), "", false)
}

func sourceParseError(filename string, diagnostics hcl.Diagnostics) error {
	location := filename

	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == hcl.DiagError && diagnostic.Subject != nil {
			position := diagnostic.Subject.Start
			location = fmt.Sprintf("%s:%d:%d", filename, position.Line, position.Column)

			break
		}
	}

	slog.Error("tofu.source_lint.parse_failed", "location", location, "err", errInvalidSource)

	return fmt.Errorf("%s: %w", location, errInvalidSource)
}

func (rules *sourceRules) repeatedLiterals() {
	const minimumOccurrences = 2

	for _, literals := range rules.literals {
		for key, locations := range literals {
			if len(locations) < minimumOccurrences {
				continue
			}

			sort.Slice(
				locations,
				func(i, j int) bool {
					if locations[i].Filename != locations[j].Filename {
						return locations[i].Filename < locations[j].Filename
					}

					return locations[i].Start.Byte < locations[j].Start.Byte
				},
			)
			category, _, _ := strings.Cut(key, "\x00")
			first := locations[0]
			message := fmt.Sprintf(
				"warning: repeated %s; first occurrence at %s:%d:%d; review a shared local",
				category,
				first.Filename,
				first.Start.Line,
				first.Start.Column,
			)

			for _, location := range locations {
				rules.findings = append(
					rules.findings,
					sourceFinding{
						location: location,
						rule:     "repeated_source_literal",
						message:  message,
						blocking: false,
					},
				)
			}
		}
	}
}

func (rules *sourceRules) print() error {
	sort.Slice(
		rules.findings,
		func(i, j int) bool {
			left, right := rules.findings[i], rules.findings[j]
			if left.location.Filename != right.location.Filename {
				return left.location.Filename < right.location.Filename
			}

			return left.location.Start.Byte < right.location.Start.Byte
		},
	)
	blocked := false

	for _, finding := range rules.findings {
		fmt.Fprintf(
			os.Stderr,
			"%s:%d:%d: %s (%s)\n",
			finding.location.Filename,
			finding.location.Start.Line,
			finding.location.Start.Column,
			finding.message,
			finding.rule,
		)
		blocked = blocked || finding.blocking
	}

	if blocked {
		return errSortedKeys
	}

	return nil
}

func skipField(blockType, name string) bool {
	return (blockType == variableBlock || blockType == "output") && name == "description" ||
		blockType == moduleBlock && name == "source" ||
		blockType == variableBlock &&
			name == sourceTypeName || blockType == "provider" && name == "version"
}

func (rules *sourceRules) body(
	body *hclsyntax.Body,
	module string,
	blockType string,
	required bool,
) error {
	var result error
	provisioner := blockType == provisionerBlock

	for name, attribute := range body.Attributes {
		if required || skipField(blockType, name) {
			continue
		}

		rules.expression(attribute.Expr, module, serviceField(name))
		if provisioner && name == "inline" {
			rules.inline(attribute.Expr, nil)
		}

		if provisioner && name == "command" {
			rules.shellExpressions([]hcl.Expression{attribute.Expr}, attribute.Expr.Range(), nil)
		}

		if provisioner && (name == "script" || name == "scripts") {
			result = errors.Join(result, rules.readScripts(attribute.Expr, module))
		}
	}

	for _, block := range body.Blocks {
		result = errors.Join(
			result,
			rules.body(block.Body, module, block.Type, required || block.Type == "terraform"),
		)
	}

	return result
}

func (rules *sourceRules) expression(expression hclsyntax.Expression, module string, service bool) {
	var keys []hcl.Range
	var services []hcl.Range
	diagnostics := hclsyntax.VisitAll(
		expression,
		func(node hclsyntax.Node) hcl.Diagnostics {
			switch expression := node.(type) {
			case *hclsyntax.ObjectConsExpr:
				services = append(services, serviceRanges(expression)...)
			case *hclsyntax.ObjectConsKeyExpr:
				keys = append(keys, expression.Range())
			case *hclsyntax.ForExpr:
				rules.sortedKeys(expression)
			case *hclsyntax.TemplateExpr:
				if !insideRanges(expression.Range(), keys) {
					rules.literal(
						expression,
						module,
						service || insideRanges(expression.Range(), services),
					)
				}
			}

			return nil
		},
	)
	if diagnostics.HasErrors() {
		slog.Error("tofu.source_lint.expression_failed", "err", diagnostics)
	}
}

func insideRanges(location hcl.Range, ranges []hcl.Range) bool {
	for _, candidate := range ranges {
		if candidate.Start.Byte <= location.Start.Byte && candidate.End.Byte >= location.End.Byte {
			return true
		}
	}

	return false
}

func (rules *sourceRules) sortedKeys(expression *hclsyntax.ForExpr) {
	sorted, found := expression.CollExpr.(*hclsyntax.FunctionCallExpr)
	if !found || sorted.Name != "sort" || len(sorted.Args) != 1 {
		return
	}

	keys, found := sorted.Args[0].(*hclsyntax.FunctionCallExpr)
	if !found || keys.Name != "keys" || len(keys.Args) != 1 {
		return
	}

	rules.findings = append(
		rules.findings,
		sourceFinding{
			location: rules.location(sorted.Range()),
			rule:     "redundant_sorted_keys",
			message:  "error: keys already returns keys in lexical order; remove sort",
			blocking: true,
		},
	)
}

func (rules *sourceRules) literal(expression *hclsyntax.TemplateExpr, module string, service bool) {
	if !expression.IsStringLiteral() {
		return
	}

	value, diagnostics := expression.Value(nil)
	if diagnostics.HasErrors() {
		return
	}

	category := literalCategory(value.AsString(), service)
	if category == "" {
		return
	}

	if rules.literals[module] == nil {
		rules.literals[module] = make(map[string][]hcl.Range)
	}

	literal := category + "\x00" + value.AsString()
	rules.literals[module][literal] = append(
		rules.literals[module][literal],
		rules.location(expression.Range()),
	)
}

func (rules *sourceRules) location(location hcl.Range) hcl.Range {
	if rules.originalLocation != nil {
		return *rules.originalLocation
	}

	return location
}

func templateText(expression *hclsyntax.TemplateExpr) string {
	var command strings.Builder

	for _, part := range expression.Parts {
		literal, ok := part.(*hclsyntax.LiteralValueExpr)
		if ok && literal.Val.Type() == cty.String {
			command.WriteString(literal.Val.AsString())
		} else {
			command.WriteString("DYNAMIC")
		}
	}

	return command.String()
}

func (rules *sourceRules) readScripts(expression hcl.Expression, module string) error {
	absoluteModule, err := filepath.Abs(module)
	if err != nil {
		slog.Error("tofu.source_lint.script_module_failed", "err", err)

		return fmt.Errorf("resolve script module: %w", err)
	}

	context := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"path": cty.ObjectVal(map[string]cty.Value{moduleBlock: cty.StringVal(absoluteModule)}),
		},
	}
	value, diagnostics := expression.Value(context)
	if diagnostics.HasErrors() || !value.IsWhollyKnown() || value.IsNull() {
		return nil
	}

	values := []cty.Value{value}
	if value.CanIterateElements() {
		values = value.AsValueSlice()
	}

	var result error

	for _, value := range values {
		if value.Type() != cty.String || value.IsNull() {
			continue
		}

		result = errors.Join(result, rules.readScript(value.AsString(), module))
	}

	return result
}

func (rules *sourceRules) readScript(path, module string) error {
	if !filepath.IsAbs(path) {
		path = filepath.Join(module, path)
	}

	if rules.scripts[path] {
		return nil
	}

	rules.scripts[path] = true
	contents, err := os.ReadFile(path)
	if err != nil {
		slog.Error("tofu.source_lint.script_read_failed", "path", path, "err", err)

		return fmt.Errorf("read provisioner script %s: %w", path, err)
	}

	rules.shell(
		string(contents),
		hcl.Range{Filename: path, Start: hcl.InitialPos, End: hcl.InitialPos},
	)

	return nil
}

var hostLiteral = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+$`)

func literalCategory(value string, service bool) string {
	if strings.ContainsAny(value, " \t\r\n") {
		return ""
	}

	if strings.Contains(value, "://") {
		return "URL"
	}

	if strings.HasPrefix(value, "/") && len(value) > 1 || strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "../") {
		return "path"
	}

	if strings.HasSuffix(value, ".service") || service && serviceIdentifier.MatchString(value) {
		return "service identifier"
	}

	if hostLiteral.MatchString(value) {
		return "hostname"
	}

	return ""
}

var (
	serviceIdentifier = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)
	serviceFields     = map[string]bool{
		"service":            true,
		"service_name":       true,
		"service_id":         true,
		"service_identifier": true,
		"unit":               true,
		"unit_name":          true,
	}
)

func serviceField(name string) bool { return serviceFields[name] }

func serviceRanges(expression *hclsyntax.ObjectConsExpr) []hcl.Range {
	var ranges []hcl.Range

	for _, item := range expression.Items {
		key, diagnostics := item.KeyExpr.Value(nil)
		if diagnostics.HasErrors() || key.IsNull() || !key.IsKnown() || key.Type() != cty.String {
			continue
		}

		if serviceField(key.AsString()) {
			ranges = append(ranges, item.ValueExpr.Range())
		}
	}

	return ranges
}
