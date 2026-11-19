package main_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

// Import a native deposed instance into the real encrypted fixture backend.
func deposeInstance(t *testing.T, root, name string) {
	t.Helper()
	exported := retirementRun(t, root, "tofu", "state", "pull")
	var state map[string]json.RawMessage
	if err := json.Unmarshal([]byte(exported), &state); err != nil {
		t.Fatal(err)
	}

	var resources []map[string]json.RawMessage
	if err := json.Unmarshal(state["resources"], &resources); err != nil {
		t.Fatal(err)
	}

	matched := false

	for _, resource := range resources {
		var resourceName string
		if err := json.Unmarshal(resource["name"], &resourceName); err != nil {
			t.Fatal(err)
		}

		if resourceName != name {
			continue
		}

		var instances []map[string]json.RawMessage
		if err := json.Unmarshal(resource["instances"], &instances); err != nil {
			t.Fatal(err)
		}

		for _, instance := range instances {
			instance["deposed"] = json.RawMessage(`"00000001"`)
			matched = true
		}

		data, err := json.Marshal(instances)
		if err != nil {
			t.Fatal(err)
		}

		resource["instances"] = data
	}

	if !matched {
		t.Fatal("expected an existing fixture instance")
	}

	data, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}

	state["resources"] = data
	prepared, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	capture := filepath.Join(t.TempDir(), "deposed.tfstate")
	retirementSave(t, capture, prepared)
	retirementRun(t, root, "tofu", "state", "push", "-force", capture)
	actual := retirementRun(t, root, "tofu", "state", "pull")
	if err := os.WriteFile(capture, []byte(actual), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := statemigration.Inspect(capture); err == nil ||
		!strings.Contains(err.Error(), "deposed instance") {
		t.Fatalf("migration inspector accepted a deposed instance: %v", err)
	}
}
