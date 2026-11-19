package sourcelint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/sourcelint"
)

func TestRunMalformedJSONInterpolation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.tofu.json")
	const source = `{"output": {"value": {
  "value": "private-fixture-value\n${1 + }"
}}}
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	err := sourcelint.Run(t.Context(), root)
	if err == nil {
		t.Fatal("source lint accepted malformed JSON interpolation")
	}

	message := err.Error()
	if !strings.Contains(message, path+":2:12: invalid OpenTofu source") ||
		strings.Contains(message, "private-fixture-value") {
		t.Fatalf("source lint returned an unexpected error: %v", err)
	}
}

func TestRunMalformedActiveSource(t *testing.T) {
	for _, fixture := range []struct {
		filename string
		inactive string
		contents string
		position string
	}{
		{"main.tofu", "main.tf", "locals {\n  secret = \"private-fixture-value\n}\n", "2:34"},
		{
			"main.tofu.json",
			"main.tf.json",
			"{\n  \"locals\": {\"secret\": \"private-fixture-value\",}\n}\n",
			"2:47",
		},
		{
			"resource.tofu.json",
			"resource.tf.json",
			"{\n  \"resource\": \"private-fixture-value\"\n}\n",
			"2:15",
		},
	} {
		t.Run(
			fixture.filename,
			func(t *testing.T) {
				root := t.TempDir()
				active := filepath.Join(root, fixture.filename)
				if err := os.WriteFile(active, []byte(fixture.contents), 0o600); err != nil {
					t.Fatal(err)
				}

				inactive := filepath.Join(root, fixture.inactive)
				if err := os.WriteFile(inactive, []byte("inactive"), 0o600); err != nil {
					t.Fatal(err)
				}

				err := sourcelint.Run(t.Context(), root)
				if err == nil {
					t.Fatal("source lint accepted malformed source")
				}

				message := err.Error()
				expected := active + ":" + fixture.position + ": invalid OpenTofu source"
				if !strings.Contains(message, expected) ||
					!strings.Contains(message, "TFLint source checks failed") ||
					strings.Contains(message, "private-fixture-value") {
					t.Fatalf("source lint returned an unexpected error: %v", err)
				}
			},
		)
	}
}
