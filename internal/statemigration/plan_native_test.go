package statemigration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

func TestCheckPlanRetainsCompleteNativeChangeDigestsWithoutBackendWrites(t *testing.T) {
	workspace := workspaceFixture(t, "source")
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false")
	backend := filepath.Join(workspace.Directory, "runtime.tfstate")
	before, err := os.ReadFile(backend)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := os.ReadFile(filepath.Join("testdata", "plan_metadata.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Directory, "extra.tf"), extra, 0o600); err != nil {
		t.Fatal(err)
	}
	private := privateFixtureDir(t)
	if err := statemigration.CheckPlan(t.Context(), workspace, private); err == nil {
		t.Fatal("CheckPlan accepted a real native managed change")
	}
	files, err := filepath.Glob(filepath.Join(private, "reports", "verification-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("CheckPlan did not retain its private native metadata report")
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Changes []struct {
			Address string `json:"address"`
			Digest  string `json:"change_digest"`
		} `json:"managed_changes"`
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Changes) != 1 {
		t.Fatal("CheckPlan omitted the real native resource change")
	}
	reference := referenceNativeChanges(t, workspace)
	for _, change := range report.Changes {
		if change.Digest != reference[change.Address] {
			t.Fatal("the native report did not hash the complete resource change")
		}
	}
	after, err := os.ReadFile(backend)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("CheckPlan changed the actual encrypted backend bytes")
	}
	assertPrivatePlanMetadataOnly(t, private)
}

func referenceNativeChanges(t *testing.T, workspace statemigration.Workspace) map[string]string {
	t.Helper()
	directory := privateFixtureDir(t)
	plan := filepath.Join(directory, "reference.tfplan")
	if err := os.WriteFile(plan, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	nativeCommand(t, workspace, "plan", "-input=false", "-out="+plan)
	command := exec.CommandContext(context.Background(), "tofu", "show", "-json", "-plan="+plan)
	command.Dir, command.Env = workspace.Directory, workspace.Environment
	var output, stderr bytes.Buffer
	command.Stdout, command.Stderr = &output, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("native reference plan decoding failed: %v", err)
	}
	var document struct {
		Changes []json.RawMessage `json:"resource_changes"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal("the native reference plan did not return supported JSON")
	}
	changes := make(map[string]string)
	for _, raw := range document.Changes {
		var address struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(raw, &address); err != nil {
			t.Fatal(err)
		}
		canonical := referenceSortedJSON(t, raw)
		sum := sha256.Sum256(canonical)
		changes[address.Address] = hex.EncodeToString(sum[:])
	}
	return changes
}

func referenceSortedJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var encoded []byte
	var err error
	switch raw[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		for key, value := range object {
			object[key] = referenceSortedJSON(t, value)
		}
		encoded, err = json.Marshal(object)
	case '[':
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			t.Fatal(err)
		}
		for index, value := range array {
			array[index] = referenceSortedJSON(t, value)
		}
		encoded, err = json.Marshal(array)
	default:
		return raw
	}
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func assertPrivatePlanMetadataOnly(t *testing.T, directory string) {
	t.Helper()
	if err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(syntheticMarker)) || bytes.Contains(data, []byte(nativeEncryptionKey)) {
			t.Fatal("the native plan metadata exposed a synthetic private value")
		}
		if strings.HasPrefix(entry.Name(), "verification-") && !strings.HasSuffix(entry.Name(), ".json") {
			t.Fatal("CheckPlan retained a raw native plan outside scratch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "reports" {
		t.Fatal("CheckPlan retained raw plan or JSON UI scratch files")
	}
}

func TestCheckPlanRefusesNativeNoopImportsAndMoves(t *testing.T) {
	for _, fixture := range []string{"plan_pending_import.tf", "plan_pending_move.tf"} {
		t.Run(fixture, func(t *testing.T) {
			workspace := workspaceFixture(t, "source")
			nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false")
			backend := filepath.Join(workspace.Directory, "runtime.tfstate")
			before, err := os.ReadFile(backend)
			if err != nil {
				t.Fatal(err)
			}
			configureNativeNoopPlan(t, workspace, fixture)
			private := privateFixtureDir(t)
			if err := statemigration.CheckPlan(t.Context(), workspace, private); err == nil || !strings.Contains(err.Error(), "would change managed resource terraform_data.pending") {
				t.Fatalf("CheckPlan accepted a native state-only managed operation: %v", err)
			}
			files, err := filepath.Glob(filepath.Join(private, "reports", "verification-*.json"))
			if err != nil || len(files) != 1 {
				t.Fatal("CheckPlan omitted its real native change report")
			}
			data, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Changes []struct {
					Actions []string `json:"actions"`
				} `json:"managed_changes"`
			}
			if json.Unmarshal(data, &report) != nil || len(report.Changes) != 1 || !slices.Equal(report.Changes[0].Actions, []string{"no-op"}) {
				t.Fatal("the real native fixture did not produce a pending no-op import or move")
			}
			if bytes.Contains(data, []byte("synthetic-private-import-identity-never-print")) {
				t.Fatal("the native report exposed its private import identity")
			}
			after, err := os.ReadFile(backend)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("the refused native no-op operation changed the actual backend")
			}
			assertPrivatePlanMetadataOnly(t, private)
		})
	}
}

func configureNativeNoopPlan(t *testing.T, workspace statemigration.Workspace, fixture string) {
	t.Helper()
	extra, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Directory, "extra.tf"), extra, 0o600); err != nil {
		t.Fatal(err)
	}
	if fixture != "plan_pending_move.tf" {
		return
	}
	main := filepath.Join(workspace.Directory, "main.tf")
	data, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), `resource "terraform_data" "original"`, `resource "terraform_data" "pending"`, 1)
	if err := os.WriteFile(main, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}
