package oracle

import (
	"os/exec"
	"slices"
	"testing"
)

// TestRouteClassifiesForms drives the embedded jinja2 oracle through Route and
// checks each form's parse flag and violating construct kinds, covering the
// banned default and presence idioms, the runtime and fact names that spare
// them, and a form jinja2 cannot parse.
func TestRouteClassifiesForms(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required: %v", err)
	}
	if err := exec.Command("python3", "-c", "import jinja2").Run(); err != nil {
		t.Fatalf("jinja2 is required for python3: %v", err)
	}
	t.Chdir(t.TempDir())

	cases := []struct {
		expr    string
		runtime []string
		parsed  bool
		kinds   []string
	}{
		{expr: "x | default('')", parsed: true, kinds: []string{"default"}},
		{expr: "x | d('')", parsed: true, kinds: []string{"default"}},
		{expr: "cmd.rc | default(1)", runtime: []string{"cmd"}, parsed: true},
		{expr: "(smtp_user | trim) | length > 0", parsed: true, kinds: []string{"length"}},
		{expr: "guests | length", parsed: true},
		{expr: "x is defined", parsed: true, kinds: []string{"presence"}},
		{expr: "x is undefined", parsed: true, kinds: []string{"presence"}},
		{expr: "x is none", parsed: true, kinds: []string{"presence"}},
		{expr: "ansible_default_ipv4 is defined", parsed: true},
		{expr: "d.get('k')", parsed: true, kinds: []string{"get"}},
		{expr: "d.get('k', 0)", parsed: true, kinds: []string{"get-default"}},
		{expr: `a + '\n' if a else ''`, parsed: true, kinds: []string{"self-ternary"}},
		{expr: "'true' if flag else 'false'", parsed: true},
		{expr: "vault_a if env == 'testbed' else vault_b", parsed: true},
		{expr: "g in groups", parsed: true, kinds: []string{"membership"}},
		{expr: "inventory_hostname in groups['adguard_servers']", parsed: true},
		{expr: "lookup('env', 'X', default='y')", parsed: true, kinds: []string{"lookup-default"}},
		{
			expr: "(groups[target_group] if target_group in groups else [target_group])" +
				" | map('extract', hostvars, 'guest_info') | select('defined') | list",
			parsed: true,
			kinds:  []string{"membership"},
		},
		{expr: "x | default(", parsed: false},
	}
	forms := make([]Form, len(cases))
	for i, testCase := range cases {
		// The lint engine always sends a list of runtime names, never null.
		runtime := append([]string{}, testCase.runtime...)
		forms[i] = Form{Expr: testCase.expr, Runtime: runtime}
	}

	results, err := Route(forms)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(results) != len(cases) {
		t.Fatalf("Route returned %d results for %d forms", len(results), len(cases))
	}
	for i, testCase := range cases {
		result := results[i]
		if result.Parsed != testCase.parsed {
			t.Errorf("%q: parsed = %v, want %v", testCase.expr, result.Parsed, testCase.parsed)
		}
		kinds := make([]string, 0, len(result.Violations))
		for _, violation := range result.Violations {
			kinds = append(kinds, violation.Kind)
		}
		slices.Sort(kinds)
		want := slices.Clone(testCase.kinds)
		slices.Sort(want)
		if !slices.Equal(kinds, want) {
			t.Errorf("%q: violation kinds = %v, want %v", testCase.expr, kinds, want)
		}
	}
}
