package sourcelint_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/sourcelint"
)

func TestRunMalformedActiveSource(t *testing.T) {
	for _, fixture := range []struct {
		filename string
		inactive string
		contents string
	}{
		{"main.tofu", "main.tf", "locals {\n  secret = \"private-fixture-value\n}\n"},
		{
			"main.tofu.json",
			"main.tf.json",
			"{\n  \"locals\": {\"secret\": \"private-fixture-value\",}\n}\n",
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
				if !strings.Contains(message, active+":2:") ||
					!strings.Contains(message, ": invalid OpenTofu source") ||
					!strings.Contains(message, "TFLint source checks failed") ||
					strings.Contains(message, "private-fixture-value") {
					t.Fatalf("source lint returned an unexpected error: %v", err)
				}
			},
		)
	}
}
