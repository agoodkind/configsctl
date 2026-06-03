package lint

import (
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// templateExpr is a Jinja expression pulled from a file with its 1-based line.
type templateExpr struct {
	line int
	text string
}

// loopTargetRE matches a Jinja for-loop header and captures its target names,
// such as `entry` in `{% for entry in items %}` or `k, v` in
// `{% for k, v in d.items() %}`.
var loopTargetRE = regexp.MustCompile(`\{%-?\s*for\s+(.+?)\s+in\s`)

// collectLoopVars adds every Jinja for-loop target name in the content to the
// runtime set. A loop value is a runtime value, so a default or presence check
// on it is allowed. The template reader has no loop scope, so the names are
// collected for the whole file, matching how register and set_fact names are
// gathered per file rather than per block.
func collectLoopVars(content string, runtime map[string]struct{}) {
	for _, match := range loopTargetRE.FindAllStringSubmatch(content, -1) {
		for target := range strings.SplitSeq(match[1], ",") {
			name := strings.Trim(strings.TrimSpace(target), "()")
			if name != "" {
				runtime[name] = struct{}{}
			}
		}
	}
}

// bareFields are the Ansible task keys whose values are bare Jinja expressions
// with no surrounding braces. `that` carries the assert module's conditions.
var bareFields = map[string]struct{}{
	"when": {}, "changed_when": {}, "failed_when": {}, "until": {}, "that": {},
}

func walkYAML(node *yaml.Node, exprs *[]templateExpr, registers map[string]struct{}) {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			walkYAML(child, exprs, registers)
		}
	case yaml.MappingNode:
		walkMapping(node, exprs, registers)
	case yaml.ScalarNode:
		*exprs = append(*exprs, spanExprs(node.Value, node.Line)...)
	case yaml.AliasNode:
		if node.Alias != nil {
			walkYAML(node.Alias, exprs, registers)
		}
	}
}

func walkMapping(node *yaml.Node, exprs *[]templateExpr, runtime map[string]struct{}) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch {
		case key == "register" && value.Kind == yaml.ScalarNode:
			runtime[value.Value] = struct{}{}
		case isModule(key, "set_fact") && value.Kind == yaml.MappingNode:
			collectMappingKeys(value, runtime)
			walkYAML(value, exprs, runtime)
		case key == "loop_control" && value.Kind == yaml.MappingNode:
			collectLoopVar(value, runtime)
			walkYAML(value, exprs, runtime)
		case isBareField(key):
			*exprs = append(*exprs, bareExprs(value)...)
		default:
			walkYAML(value, exprs, runtime)
		}
	}
}

// collectMappingKeys records every key of a mapping as a runtime name. The keys
// of a set_fact block are runtime values, so a defensive read of one elsewhere
// is allowed.
func collectMappingKeys(node *yaml.Node, runtime map[string]struct{}) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		runtime[node.Content[i].Value] = struct{}{}
	}
}

// collectLoopVar records the loop_var name from a loop_control block, since a
// loop value is a runtime value.
func collectLoopVar(node *yaml.Node, runtime map[string]struct{}) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "loop_var" && node.Content[i+1].Kind == yaml.ScalarNode {
			runtime[node.Content[i+1].Value] = struct{}{}
		}
	}
}

func isBareField(key string) bool {
	_, ok := bareFields[key]
	return ok
}

// isModule reports whether a mapping key names a module, accepting both the
// short name and the ansible.builtin fully qualified form.
func isModule(key, name string) bool {
	return key == name || key == "ansible.builtin."+name
}

// bareExprs treats a node's value as a bare Jinja expression, descending a
// sequence so a list-form `when` yields one expression per item.
func bareExprs(node *yaml.Node) []templateExpr {
	switch node.Kind {
	case yaml.ScalarNode:
		if strings.TrimSpace(node.Value) == "" {
			return nil
		}
		return []templateExpr{{line: node.Line, text: node.Value}}
	case yaml.SequenceNode:
		var out []templateExpr
		for _, child := range node.Content {
			out = append(out, bareExprs(child)...)
		}
		return out
	case yaml.DocumentNode, yaml.MappingNode, yaml.AliasNode:
		return nil
	}
	return nil
}

// rawBlockRE matches a Jinja `{% raw %}...{% endraw %}` region, whose body is
// literal text and must not be read as Jinja. It accepts whitespace-control
// markers and spans newlines.
var rawBlockRE = regexp.MustCompile(`(?s)\{%-?\s*raw\s*-?%\}.*?\{%-?\s*endraw\s*-?%\}`)

// stripRawBlocks blanks every `{% raw %}` region so its literal braces are not
// extracted as expressions, keeping the newline count so later line numbers
// stay correct.
func stripRawBlocks(text string) string {
	return rawBlockRE.ReplaceAllStringFunc(text, func(match string) string {
		return strings.Repeat("\n", strings.Count(match, "\n"))
	})
}

// spanExprs extracts the `{{ }}` output expressions and the `{% if/elif/set %}`
// control expressions from a string, with each line resolved from baseLine.
// Content inside a `{% raw %}` block is literal and is dropped first.
func spanExprs(text string, baseLine int) []templateExpr {
	text = stripRawBlocks(text)
	out := delimited(text, baseLine, "{{", "}}", nil)
	return append(out, delimited(text, baseLine, "{%", "%}", controlExpr)...)
}

// controlExpr reduces a control-structure body to the expression it tests,
// returning empty for structures that carry no input expression.
func controlExpr(body string) string {
	switch {
	case strings.HasPrefix(body, "if "):
		return strings.TrimSpace(body[len("if "):])
	case strings.HasPrefix(body, "elif "):
		return strings.TrimSpace(body[len("elif "):])
	case strings.HasPrefix(body, "set "):
		if _, rhs, ok := strings.Cut(body, "="); ok {
			return strings.TrimSpace(rhs)
		}
		return ""
	default:
		return ""
	}
}

func delimited(text string, baseLine int, opening, closing string, transform func(string) string) []templateExpr {
	var out []templateExpr
	cursor := 0
	for {
		start := strings.Index(text[cursor:], opening)
		if start < 0 {
			return out
		}
		start += cursor
		end := strings.Index(text[start:], closing)
		if end < 0 {
			return out
		}
		end += start
		inner := trimMarkers(text[start+len(opening) : end])
		if transform != nil {
			inner = transform(inner)
		}
		if inner != "" {
			out = append(out, templateExpr{
				line: baseLine + strings.Count(text[:start], "\n"),
				text: inner,
			})
		}
		cursor = end + len(closing)
	}
}

// trimMarkers strips surrounding whitespace and the `-` whitespace-control
// markers from a delimited body.
func trimMarkers(body string) string {
	body = strings.TrimSpace(body)
	body = strings.TrimPrefix(body, "-")
	body = strings.TrimSuffix(body, "-")
	return strings.TrimSpace(body)
}
