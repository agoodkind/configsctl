package tofuplansource_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/tofuplansource"
)

func planFile(t *testing.T) string {
	t.Helper()
	// os.UserCacheDir uses HOME on Darwin and XDG_CACHE_HOME on Linux.
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	path := filepath.Join(t.TempDir(), "saved.tfplan")
	if err := os.WriteFile(path, []byte("synthetic saved plan bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func installedPlanSource() tofuplansource.Source {
	return tofuplansource.Source{
		Status:         "committed",
		Commit:         "0123456789012345678901234567890123456789",
		Workspace:      "workspace",
		Kind:           "installed",
		Root:           "/opt/configs/idrac-certificate",
		Artifact:       "poweredge/idrac-certificate",
		Node:           "poweredge",
		Environment:    "testbed",
		ManifestSHA256: strings.Repeat("a", 64),
	}
}

func TestSavedPlanRequiresExactInstalledArtifact(t *testing.T) {
	path := planFile(t)
	source := installedPlanSource()
	if err := tofuplansource.Record(path, source); err != nil {
		t.Fatal(err)
	}

	if err := tofuplansource.Require(path, source); err != nil {
		t.Fatal(err)
	}

	for _, field := range []string{
		"commit",
		"workspace",
		"kind",
		"root",
		"artifact",
		"node",
		"environment",
		"manifest",
	} {
		t.Run(
			field,
			func(t *testing.T) {
				current := installedPlanSource()
				switch field {
				case "commit":
					current.Commit = strings.Repeat("b", 40)
				case "workspace":
					current.Workspace = "another/workspace"
				case "kind":
					current.Kind = ""
				case "root":
					current.Root = "/opt/configs/another"
				case "artifact":
					current.Artifact = "poweredge/another"
				case "node":
					current.Node = "another-node"
				case "environment":
					current.Environment = "another-environment"
				case "manifest":
					current.ManifestSHA256 = strings.Repeat("b", 64)
				}

				if err := tofuplansource.Require(path, current); err == nil ||
					!strings.Contains(err.Error(), "recorded source does not match") {
					t.Fatalf("the saved plan accepted a different %s: %v", field, err)
				}
			},
		)
	}
}

func TestSavedPlanRequiresExactBytes(t *testing.T) {
	path := planFile(t)
	source := installedPlanSource()
	if err := tofuplansource.Record(path, source); err != nil {
		t.Fatal(err)
	}

	copyPath := filepath.Join(t.TempDir(), "private.tfplan")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(copyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tofuplansource.Require(copyPath, source); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(copyPath, append(data, []byte("changed")...), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := tofuplansource.Require(copyPath, source); err == nil ||
		!strings.Contains(err.Error(), "source record is unavailable") {
		t.Fatalf("the source cache accepted changed plan bytes: %v", err)
	}
}

func TestSavedPlanReadsExistingGitRecord(t *testing.T) {
	path := planFile(t)
	metadata, err := tofuplansource.Path(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Dir(metadata), 0o700); err != nil {
		t.Fatal(err)
	}

	legacy := `{"status":"committed","commit":"0123456789012345678901234567890123456789","workspace":"workspaces/alpha"}`
	if err := os.WriteFile(metadata, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	current := tofuplansource.Source{
		Status:    "committed",
		Commit:    "0123456789012345678901234567890123456789",
		Workspace: "workspaces/alpha",
	}
	if err := tofuplansource.Require(path, current); err != nil {
		t.Fatal(err)
	}
}
