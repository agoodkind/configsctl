// Package lint detects Ansible input-default and presence checks by parsing
// Jinja expressions into a real AST and classifying the operand each construct
// reads. A default or presence check on a declared input variable is a
// violation, because an input value must be deterministic. The same check on a
// registered result, a gathered fact, a set_fact value, or a loop value is a
// runtime value and is allowed.
package lint

import (
	"strings"

	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

// factPrefix marks the Ansible fact namespace. A root with this prefix is a
// gathered fact, so a defensive read of it is allowed.
const factPrefix = "ansible_"

// factRoots are the fixed runtime and magic names whose presence or shape is
// external. The caller adds per-file register, set_fact, and loop names.
var factRoots = map[string]struct{}{
	"ansible_facts": {}, "ansible_local": {}, "hostvars": {}, "groups": {},
	"group_names": {}, "inventory_hostname": {}, "inventory_hostname_short": {},
	"ansible_play_hosts": {}, "ansible_play_hosts_all": {}, "play_hosts": {},
	"ansible_play_batch": {}, "ansible_check_mode": {}, "ansible_run_tags": {},
	"ansible_skip_tags": {}, "ansible_version": {}, "ansible_date_time": {},
	"item": {}, "ansible_loop": {}, "ansible_loop_var": {}, "omit": {},
}

var (
	defaultFilters       = map[string]struct{}{"default": {}, "d": {}}
	lengthFilters        = map[string]struct{}{"length": {}, "count": {}}
	presenceTests        = map[string]struct{}{"defined": {}, "undefined": {}, "none": {}}
	membershipContainers = map[string]struct{}{"groups": {}, "hostvars": {}, "vars": {}}
	comparisonOps        = map[string]struct{}{
		"==": {}, "!=": {}, "<": {}, ">": {}, "<=": {}, ">=": {},
	}
	lookupFunctions = map[string]struct{}{"lookup": {}, "query": {}, "q": {}}
)

// Construct is one default or presence form found in an expression. Kind names
// the form and Root is the operand variable it reads, empty when the form has no
// classifiable operand (a lookup default).
type Construct struct {
	Kind string
	Root string
}

// parseTemplate lexes and parses a Jinja source string into a template AST. It
// reports false when the source does not parse, which the caller treats as
// nothing found rather than an error, the same as the Python engine.
func parseTemplate(source string) (*nodes.Template, bool) {
	cfg := config.New()
	stream := tokens.Lex(source, cfg)
	tmpl, err := parser.NewParser("lint", stream, cfg, nil, nil).Parse()
	if err != nil {
		return nil, false
	}
	return tmpl, true
}

// walk visits node and every descendant in depth-first order. gonja ships no
// usable AST walker (its own Walk handles only Template and Wrapper), so this
// covers the expression node types directly.
func walk(node nodes.Node, visit func(nodes.Node)) {
	if node == nil {
		return
	}
	visit(node)
	for _, child := range children(node) {
		walk(child, visit)
	}
}

// children returns the non-nil child nodes of a gonja AST node.
func children(node nodes.Node) []nodes.Node {
	switch typed := node.(type) {
	case *nodes.Template:
		return typed.Nodes
	case *nodes.Wrapper:
		return typed.Nodes
	case *nodes.Output:
		return compact(typed.Expression, typed.Condition, typed.Alternative)
	case *nodes.FilteredExpression:
		out := compact(typed.Expression)
		for _, filter := range typed.Filters {
			out = append(out, exprs(filter.Args)...)
			out = append(out, kwargs(filter.Kwargs)...)
		}
		return out
	case *nodes.TestExpression:
		out := compact(typed.Expression)
		if typed.Test != nil {
			out = append(out, exprs(typed.Test.Args)...)
			out = append(out, kwargs(typed.Test.Kwargs)...)
		}
		return out
	case *nodes.BinaryExpression:
		return compact(typed.Left, typed.Right)
	case *nodes.UnaryExpression:
		return compact(typed.Term)
	case *nodes.Negation:
		return compact(typed.Term)
	case *nodes.Call:
		out := compact(typed.Func)
		out = append(out, exprs(typed.Args)...)
		return append(out, kwargs(typed.Kwargs)...)
	case *nodes.GetAttribute:
		return compact(typed.Node)
	case *nodes.GetItem:
		return compact(typed.Node, typed.Arg)
	case *nodes.GetSlice:
		return compact(typed.Node, typed.Start, typed.End)
	case *nodes.List:
		return exprs(typed.Val)
	case *nodes.Tuple:
		return exprs(typed.Val)
	case *nodes.Dict:
		return dictChildren(typed)
	default:
		return nil
	}
}

func dictChildren(dict *nodes.Dict) []nodes.Node {
	out := make([]nodes.Node, 0, len(dict.Pairs)*2)
	for _, pair := range dict.Pairs {
		out = append(out, pair.Key, pair.Value)
	}
	return out
}

func compact(candidates ...nodes.Node) []nodes.Node {
	out := make([]nodes.Node, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil {
			out = append(out, candidate)
		}
	}
	return out
}

func exprs(list []nodes.Expression) []nodes.Node {
	out := make([]nodes.Node, 0, len(list))
	for _, expr := range list {
		if expr != nil {
			out = append(out, expr)
		}
	}
	return out
}

func kwargs(values map[string]nodes.Expression) []nodes.Node {
	out := make([]nodes.Node, 0, len(values))
	for _, expr := range values {
		if expr != nil {
			out = append(out, expr)
		}
	}
	return out
}

// rootName resolves a node to the base variable it reads, descending through
// attribute access, subscripts, filters, and calls. It returns the empty string
// when the base is not a name.
func rootName(node nodes.Node) string {
	for {
		switch current := node.(type) {
		case *nodes.Name:
			return current.Name.Val
		case *nodes.Variable:
			if len(current.Parts) > 0 {
				return current.Parts[0].S
			}
			return ""
		case *nodes.GetAttribute:
			node = current.Node
		case *nodes.GetItem:
			node = current.Node
		case *nodes.FilteredExpression:
			node = current.Expression
		case *nodes.Call:
			node = current.Func
		default:
			return ""
		}
	}
}

// Analyze parses a Jinja source string, returns every default or presence
// construct it contains, and reports whether the source parsed. A false parse
// flag marks a form gonja could not read, which the caller lists for review
// rather than silently dropping.
func Analyze(source string) ([]Construct, bool) {
	tmpl, ok := parseTemplate(source)
	if !ok {
		return nil, false
	}
	var found []Construct
	walk(tmpl, func(node nodes.Node) {
		found = append(found, detect(node)...)
	})
	return found, true
}

// detect returns the constructs carried by a single node.
func detect(node nodes.Node) []Construct {
	switch typed := node.(type) {
	case *nodes.FilteredExpression:
		return detectDefault(typed)
	case *nodes.TestExpression:
		return detectTest(typed)
	case *nodes.Call:
		return detectCall(typed)
	case *nodes.BinaryExpression:
		return detectBinary(typed)
	case *nodes.Output:
		return detectTernary(typed)
	default:
		return nil
	}
}

func detectDefault(expr *nodes.FilteredExpression) []Construct {
	for _, filter := range expr.Filters {
		if _, ok := defaultFilters[filter.Name]; ok {
			return []Construct{{Kind: "default", Root: rootName(expr.Expression)}}
		}
	}
	return nil
}

// detectTest handles `is` tests. Presence tests (defined, undefined, none) and
// membership against a bare container (`x in groups`) both read the operand
// defensively. gonja parses `in` as a test, and `not in` wraps it in a Negation
// the walker descends, so both are caught here.
func detectTest(expr *nodes.TestExpression) []Construct {
	name := expr.Test.Name
	if _, ok := presenceTests[name]; ok {
		return []Construct{{Kind: "presence", Root: rootName(expr.Expression)}}
	}
	if name == "in" && len(expr.Test.Args) == 1 {
		if container, ok := expr.Test.Args[0].(*nodes.Name); ok {
			if _, isContainer := membershipContainers[container.Name.Val]; isContainer {
				return []Construct{{Kind: "membership", Root: rootName(expr.Expression)}}
			}
		}
	}
	return nil
}

func detectCall(call *nodes.Call) []Construct {
	if attr, ok := call.Func.(*nodes.GetAttribute); ok && attr.Attribute == "get" {
		kind := "get"
		if len(call.Args) >= 2 {
			kind = "get-default"
		}
		return []Construct{{Kind: kind, Root: rootName(attr.Node)}}
	}
	if name, ok := call.Func.(*nodes.Name); ok {
		if _, isLookup := lookupFunctions[name.Name.Val]; isLookup {
			if _, hasDefault := call.Kwargs["default"]; hasDefault {
				return []Construct{{Kind: "lookup-default", Root: ""}}
			}
		}
	}
	return nil
}

func detectBinary(expr *nodes.BinaryExpression) []Construct {
	op := expr.Operator.Token.Val
	if _, ok := comparisonOps[op]; !ok {
		return nil
	}
	if root, found := lengthRoot(expr.Left); found {
		return []Construct{{Kind: "length", Root: root}}
	}
	if root, found := lengthRoot(expr.Right); found {
		return []Construct{{Kind: "length", Root: root}}
	}
	return nil
}

// lengthRoot reports the operand root when an expression is a filter chain that
// ends in a length or count filter.
func lengthRoot(expr nodes.Expression) (string, bool) {
	filtered, ok := expr.(*nodes.FilteredExpression)
	if !ok {
		return "", false
	}
	for _, filter := range filtered.Filters {
		if _, isLength := lengthFilters[filter.Name]; isLength {
			return rootName(filtered.Expression), true
		}
	}
	return "", false
}

func detectTernary(out *nodes.Output) []Construct {
	if out.Condition == nil {
		return nil
	}
	root := rootName(out.Condition)
	if root == "" {
		return nil
	}
	if nameAppears(out.Expression, root) {
		return []Construct{{Kind: "self-ternary", Root: root}}
	}
	return nil
}

// nameAppears reports whether a variable name is referenced anywhere under a node.
func nameAppears(node nodes.Node, name string) bool {
	found := false
	walk(node, func(current nodes.Node) {
		if named, ok := current.(*nodes.Name); ok && named.Name.Val == name {
			found = true
		}
	})
	return found
}

// IsViolation reports whether a construct violates the rule. A lookup default
// always does. Any other construct violates unless its root is a runtime value,
// which is a register, set_fact, or loop name in runtime, or a fixed fact name.
func IsViolation(construct Construct, runtime map[string]struct{}) bool {
	if construct.Kind == "lookup-default" {
		return true
	}
	// An unresolvable operand is a complex expression, a registered result, or a
	// gonja-vs-Jinja precedence split, not a plain input variable. A genuine
	// input-variable read always resolves to a name, so sparing this cannot hide
	// an input-variable violation.
	if construct.Root == "" {
		return false
	}
	if _, ok := runtime[construct.Root]; ok {
		return false
	}
	if _, ok := factRoots[construct.Root]; ok {
		return false
	}
	return !strings.HasPrefix(construct.Root, factPrefix)
}
