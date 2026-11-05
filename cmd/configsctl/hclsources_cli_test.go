package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type artifactSource struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Content     string `json:"content"`
}

func hclFixture(t *testing.T, name string) artifactSource {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "hclsources", name))
	if err != nil {
		t.Fatal(err)
	}
	return artifactSource{Source: "source/" + name, Destination: "/installed/" + name, Content: string(content)}
}

func runHCLCheck(t *testing.T, files []artifactSource, want string) {
	t.Helper()
	request, err := json.Marshal(map[string][]artifactSource{"files": files})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	filename := filepath.Join(dir, "request.json")
	writeFixtureFile(t, filename, string(request))
	runHCLCommand(t, dir, want, filename)
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, request) {
		t.Fatal("checker changed the input request")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("checker created a file beside the request")
	}
}

func runHCLCommand(t *testing.T, dir, want string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), configsctlBinary, append([]string{"lint-hcl-sources"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{}
	result, err := cmd.CombinedOutput()
	if want == "" {
		if err != nil || len(result) != 0 {
			t.Fatalf("checker returned %v, output %q", err, result)
		}
		return
	}
	if err == nil || !strings.Contains(string(result), want) {
		t.Fatalf("checker returned %v, output %q, want error containing %q", err, result, want)
	}
	if bytes.Contains(result, []byte("private_value")) || bytes.Contains(result, []byte("vault_demo")) {
		t.Fatal("checker printed protected source text")
	}
}

func TestHCLSourcePolicy(t *testing.T) {
	for _, test := range []struct{ name, rule string }{
		{"reference.tf", ""},
		{"heredoc.tf", ""},
		{"dynamic-reference.tf", ""},
		{"encoded-heredoc.tf", "outside a sensitive"},
		{"for-shadow.tf", "outside a sensitive"},
		{"template-shadow.tf", "outside a sensitive"},
		{"dynamic-shadow.tf", "outside a sensitive"},
		{"literal.tf", "outside a sensitive"},
		{"index.tf", "outside a sensitive"},
		{"escape.tf", "invalid native HCL"},
		{"comment.tf", "outside a sensitive"},
		{"marker.tf", "unresolved template marker"},
		{"label.tf", "outside a sensitive"},
		{"default.tf", "without a default"},
		{"nonsensitive.tf", "sensitive = true"},
		{"extra.tf", "exactly one variable label"},
		{"nested.tf", "outside a sensitive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runHCLCheck(t, []artifactSource{hclFixture(t, test.name), hclFixture(t, "variable.tf")}, test.rule)
		})
	}
}

func TestHCLSourceModule(t *testing.T) {
	declaration, reference := hclFixture(t, "variable.tf"), hclFixture(t, "reference.tf")
	t.Run("undeclared", func(t *testing.T) { runHCLCheck(t, []artifactSource{reference}, "source/reference.tf:1,") })
	for _, test := range []struct{ source, destination, rule string }{
		{"sibling/main.tf", reference.Destination, "outside a sensitive"},
		{reference.Source, "/installed/child/main.tf", "outside a sensitive"},
		{reference.Source, "/etc/example.toml", "absolute native OpenTofu destination"},
		{reference.Source, "/etc/example.service", "absolute native OpenTofu destination"},
	} {
		t.Run(test.source+test.destination, func(t *testing.T) {
			file := reference
			file.Source, file.Destination = test.source, test.destination
			runHCLCheck(t, []artifactSource{declaration, file}, test.rule)
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		other := declaration
		other.Destination = "/installed/duplicate.tf"
		runHCLCheck(t, []artifactSource{declaration, other}, "only once per installed module")
	})
	t.Run("preferred file", func(t *testing.T) {
		preferred := hclFixture(t, "empty.tofu")
		preferred.Destination = "/installed/variable.tofu"
		runHCLCheck(t, []artifactSource{declaration, preferred, reference}, "outside a sensitive")
	})
}

func TestHCLSourceOverrides(t *testing.T) {
	requireTofu(t)
	for _, name := range []string{"override.tf", "certificate_override.tf", "override.tofu", "certificate_override.tofu"} {
		t.Run(name, func(t *testing.T) {
			declaration := hclFixture(t, "variable.tf")
			reference := hclFixture(t, "reference.tf")
			override := declaration
			override.Destination = "/installed/" + name
			dir := t.TempDir()
			writeFixtureFile(t, filepath.Join(dir, name), override.Content)
			writeFixtureFile(t, filepath.Join(dir, "reference.tf"), reference.Content)
			cmd := exec.CommandContext(t.Context(), "tofu", "validate", "-no-color")
			cmd.Dir = dir
			cmd.Env = []string{"HOME=" + dir}
			result, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(result, []byte("Missing base variable declaration to override")) {
				t.Fatalf("OpenTofu returned %v, output %q", err, result)
			}
			runHCLCheck(t, []artifactSource{reference, override}, "primary file")
			base := hclFixture(t, "output.tf")
			override = hclFixture(t, "output-reference.tf")
			override.Destination = "/installed/" + name
			runHCLCheck(t, []artifactSource{override, declaration, base}, "")
		})
	}
}

func TestHCLSourceRequest(t *testing.T) {
	dir := t.TempDir()
	runHCLCommand(t, dir, "usage:")
	runHCLCommand(t, dir, "usage:", "first", "second")
	runHCLCommand(t, dir, "read HCL source request", filepath.Join(dir, "missing"))
	for _, test := range []struct{ name, rule string }{
		{"duplicate-field.json", "unique fields"},
		{"wrong-type.json", "required field types"},
		{"unknown.json", "required field types"},
		{"empty.json", "nonempty files"},
		{"missing-content.json", "content as a string"},
		{"null-content.json", "content as a string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename, err := filepath.Abs(filepath.Join("testdata", "hclsources", test.name))
			if err != nil {
				t.Fatal(err)
			}
			runHCLCommand(t, dir, test.rule, filename)
		})
	}
}

// Compare the policy with the values that OpenTofu derives from the same source.
func TestHCLSourceOpenTofuTemplates(t *testing.T) {
	requireTofu(t)
	for _, test := range []struct{ fixture, expression, value string }{
		{"heredoc.tf", "nonsensitive(chomp(local.result))", "EXAMPLE"},
		{"encoded-heredoc.tf", "chomp(local.result)", "VAULT_PRIVATE_VALUE"},
	} {
		t.Run(test.fixture, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range []string{"variable.tf", test.fixture} {
				writeFixtureFile(t, filepath.Join(dir, name), hclFixture(t, name).Content)
			}
			cmd := exec.CommandContext(t.Context(), "tofu", "console", "-no-color", "-var", "vault_demo=example")
			cmd.Dir = dir
			cmd.Env = []string{"HOME=" + dir}
			cmd.Stdin = strings.NewReader(test.expression + "\n")
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("tofu console returned %v: %s", err, output)
			}
			if strings.TrimSpace(string(output)) != `"`+test.value+`"` {
				t.Fatalf("unexpected OpenTofu result %q", output)
			}
		})
	}
}
