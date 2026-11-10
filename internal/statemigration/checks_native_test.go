package statemigration_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

func TestRepeatedNativePullPreservesCheckedStateProof(t *testing.T) {
	workspace := workspaceFixture(t, "source")
	configuration, err := os.ReadFile(filepath.Join("testdata", "checks.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Directory, "checks.tf"), configuration, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	private := privateFixtureDir(t)
	first := filepath.Join(private, "first.tfstate")
	before, err := statemigration.Pull(t.Context(), workspace, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Records) != 4 {
		t.Fatal("the native fixture did not create its checked resource instances")
	}
	for index := range 12 {
		path := filepath.Join(private, fmt.Sprintf("repeated-%02d.tfstate", index))
		actual, err := statemigration.Pull(t.Context(), workspace, path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, actual) {
			t.Fatalf("repeated native pull changed verification metadata; differing sections: %v", differentNativeSections(t, first, path))
		}
	}
	workspace.Environment = append(slices.Clone(workspace.Environment), `TF_VAR_fixture_order=["second","first"]`)
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	ordered, err := statemigration.Pull(t.Context(), workspace, filepath.Join(private, "ordered.tfstate"))
	if err != nil || ordered.Content == before.Content || ordered.Digest == before.Digest {
		t.Fatalf("verification ignored a native change to attribute array order: %v", err)
	}
	workspace.Environment = append(slices.Clone(workspace.Environment), "TF_VAR_fixture_check=false")
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	failed, err := statemigration.Pull(t.Context(), workspace, filepath.Join(private, "failed-check.tfstate"))
	if err != nil || failed.Content == ordered.Content || failed.Digest != ordered.Digest {
		t.Fatalf("verification ignored changed native check results or changed provider records: %v", err)
	}
}

func differentNativeSections(t *testing.T, before, after string) []string {
	t.Helper()
	first := nativeSectionHashes(t, before)
	second := nativeSectionHashes(t, after)
	var changed []string
	for key, value := range first {
		if second[key] != value {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}

func nativeSectionHashes(t *testing.T, path string) map[string][sha256.Size]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(data, &sections); err != nil {
		t.Fatal(err)
	}
	if len(sections["check_results"]) == 0 {
		t.Fatal("the native fixture did not record its checks")
	}
	proofs := make(map[string][sha256.Size]byte)
	for key, value := range sections {
		proofs[key] = sha256.Sum256(value)
	}
	return proofs
}
