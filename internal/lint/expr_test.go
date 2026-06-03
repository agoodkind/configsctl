package lint

import (
	"sort"
	"testing"
)

// runtimeSet builds a runtime-name set for a test case.
func runtimeSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// violatingKinds returns the sorted kinds of the violating constructs found in a
// bare expression, classified against the given runtime names.
func violatingKinds(t *testing.T, expr string, runtime map[string]struct{}) []string {
	t.Helper()
	constructs, _ := Analyze("{{ " + expr + " }}")
	var kinds []string
	for _, construct := range constructs {
		if IsViolation(construct, runtime) {
			kinds = append(kinds, construct.Kind)
		}
	}
	sort.Strings(kinds)
	return kinds
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFindConstructs checks each construct form against its expected
// violating-or-spared verdict, covering the default and presence idioms the
// linter bans and the legitimate uses it must leave alone.
func TestFindConstructs(t *testing.T) {
	cases := []struct {
		expr    string
		runtime map[string]struct{}
		want    []string
	}{
		{"x | default('')", runtimeSet(), []string{"default"}},
		{"x | d('')", runtimeSet(), []string{"default"}},
		{"cmd.rc | default(1)", runtimeSet("cmd"), nil},
		{"(smtp_user | trim) | length > 0", runtimeSet(), []string{"length"}},
		{"guests | length", runtimeSet(), nil},
		{"x is defined", runtimeSet(), []string{"presence"}},
		{"x is undefined", runtimeSet(), []string{"presence"}},
		{"x is none", runtimeSet(), []string{"presence"}},
		{"ansible_default_ipv4 is defined", runtimeSet(), nil},
		{"d.get('k')", runtimeSet(), []string{"get"}},
		{"d.get('k', 0)", runtimeSet(), []string{"get-default"}},
		{"a + '\\n' if a else ''", runtimeSet(), []string{"self-ternary"}},
		{"'true' if flag else 'false'", runtimeSet(), nil},
		{"vault_a if env == 'testbed' else vault_b", runtimeSet(), nil},
		{"g in groups", runtimeSet(), []string{"membership"}},
		{"inventory_hostname in groups['consul_servers']", runtimeSet(), nil},
		{"lookup('env', 'X', default='y')", runtimeSet(), []string{"lookup-default"}},
	}

	for _, testCase := range cases {
		got := violatingKinds(t, testCase.expr, testCase.runtime)
		if !equalStrings(got, testCase.want) {
			t.Errorf("%q: got %v, want %v", testCase.expr, got, testCase.want)
		}
	}
}
