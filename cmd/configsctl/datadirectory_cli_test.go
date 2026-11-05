//go:build unix

package main_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeExecutionUsesExternalDataDirectory(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%t", relative), func(t *testing.T) {
			fixture := newDirectFixture(t, explicitSettings)
			installNativeConfiguration(t, fixture.tree)
			root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
			stateDirectory := t.TempDir()
			dataDirectory := filepath.Join(stateDirectory, "terraform")
			if err := os.Mkdir(dataDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			backend := nativeFixtureFile(t, "external-backend.tf")
			writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(backend, filepath.Join(stateDirectory, "default.tfstate"), filepath.Join(stateDirectory, "states")))
			selected := dataDirectory
			if relative {
				var err error
				selected, err = filepath.Rel(root, dataDirectory)
				if err != nil {
					t.Fatal(err)
				}
			}
			fixture.extraEnv = []string{"TF_DATA_DIR=" + selected}
			for _, path := range []string{filepath.Join(root, "backend.tf"), filepath.Join(root, "main.tf"), filepath.Join(root, "item/main.tf")} {
				if err := os.Chmod(path, 0o444); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{root, filepath.Join(root, "item")} {
				if err := os.Chmod(path, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
			}
			requireSuccess(t, fixture.mutate(t, "init", "-input=false"))
			requireSuccess(t, fixture.mutate(t, "workspace", "new", "runtime"))
			requireSuccess(t, fixture.mutate(t, "plan", "-input=false"))
			requireSuccess(t, fixture.apply(t, "-auto-approve"))
			state := fixture.mutate(t, "state", "list")
			requireSuccess(t, state)
			if !strings.Contains(state.stdout, `module.base["guest_alpha"].terraform_data.item`) {
				t.Fatal("native apply did not update the selected external workspace")
			}
			requireFile(t, filepath.Join(stateDirectory, "states/runtime/terraform.tfstate"))
			if err := os.Chmod(dataDirectory, 0o755); err != nil {
				t.Fatal(err)
			}
			refused := fixture.apply(t, "-auto-approve")
			if refused.exitCode == 0 || !strings.Contains(refused.stderr, "DataDirectory") || !strings.Contains(refused.stderr, "group and other users must not have access") {
				t.Fatalf("native apply accepted an unsafe external data directory: %s", refused.stderr)
			}
		})
	}
}
