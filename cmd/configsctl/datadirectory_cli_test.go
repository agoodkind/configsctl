//go:build unix

package main_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
			selection := fixture.mutate(t, "-chdir=item", "workspace", "show")
			requireSuccess(t, selection)
			if strings.TrimSpace(selection.stdout) != "runtime" {
				t.Fatalf("read-only -chdir used another data directory: %q", selection.stdout)
			}
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
			for _, args := range [][]string{{"init", "-input=false"}, {"plan", "-input=false"}, {"show", "-json"}, {"workspace", "show"}, {"apply", "-auto-approve"}} {
				refused := fixture.mutate(t, args...)
				if refused.exitCode == 0 || !strings.Contains(refused.stderr, "DataDirectory") || !strings.Contains(refused.stderr, "group and other users must not have access") {
					t.Fatalf("native %s accepted an unsafe external data directory: %s", args[0], refused.stderr)
				}
			}
			info, err := os.Stat(dataDirectory)
			if err != nil || info.Mode().Perm() != 0o755 {
				t.Fatal("native preflight changed the unsafe directory permissions")
			}
		})
	}
}

func TestNativeDataDirectoryPreflightPreventsCredentialPreparation(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	installNativeCredential(t, fixture, server)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "environment"), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.extraEnv = []string{"TF_DATA_DIR=" + directory}
	refused := fixture.apply(t, "-auto-approve")
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "DataDirectory") || !strings.Contains(refused.stderr, "group and other users must not have access") {
		t.Fatalf("native apply did not validate its data directory before reading workspace selection: %s", refused.stderr)
	}
	if requests.Load() != 0 {
		t.Fatal("native apply contacted the credential controller before validating its data directory")
	}
	requireNoFile(t, filepath.Join(fixture.lockDir, "deploy.lock"))
}
