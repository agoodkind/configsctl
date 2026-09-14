package oracle

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// requireOracle fails when python3 cannot import jinja2 in isolated mode, which
// is how Route runs the oracle. A skip would pass while the oracle path is broken.
func requireOracle(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatalf("python3 is required: %v", err)
	}
	if err := exec.Command("python3", "-I", "-c", "import jinja2").Run(); err != nil {
		t.Fatalf("jinja2 is required for python3 in isolated mode: %v", err)
	}
}

// TestRouteClassifiesForms drives the embedded jinja2 oracle through Route and
// checks each form's parse flag and violating construct kinds, covering the
// banned default and presence idioms, the runtime and fact names that spare
// them, and a form jinja2 cannot parse.
func TestRouteClassifiesForms(t *testing.T) {
	requireOracle(t)
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

// TestRouteIgnoresModulesPlantedInTheTempDirectory plants a hostile json.py in
// a directory, then points both TMPDIR and PYTHONPATH at it. On a shared host
// another user can write to the temp directory, so the oracle must never import
// a module from there or from the caller's Python environment variables.
func TestRouteIgnoresModulesPlantedInTheTempDirectory(t *testing.T) {
	requireOracle(t)
	hostile, err := os.ReadFile(filepath.Join("testdata", "json.py"))
	if err != nil {
		t.Fatalf("read hostile module: %v", err)
	}
	planted := t.TempDir()
	if err := os.WriteFile(filepath.Join(planted, "json.py"), hostile, 0o600); err != nil {
		t.Fatalf("plant hostile module: %v", err)
	}
	marker := filepath.Join(t.TempDir(), "hijacked")
	t.Setenv("CONFIGSCTL_HIJACK_MARKER", marker)
	t.Setenv("TMPDIR", planted)
	t.Setenv("PYTHONPATH", planted)

	results, routeErr := Route([]Form{{Expr: "x | default('')", Runtime: []string{}}})
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("Route imported the json.py planted in %s", planted)
	}
	if routeErr != nil {
		t.Fatalf("Route: %v", routeErr)
	}
	if len(results) != 1 || !results[0].Parsed || len(results[0].Violations) != 1 {
		t.Fatalf("Route results = %+v, want one parsed form with one violation", results)
	}
}
