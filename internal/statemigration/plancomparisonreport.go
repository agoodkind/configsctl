package statemigration

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
)

type planDifference struct {
	Kind           string `json:"kind"`
	Workspace      string `json:"workspace"`
	Address        string `json:"address"`
	Classification string `json:"classification"`
	Before         string `json:"old_digest"`
	After          string `json:"new_digest"`
}

type planComparisonError struct {
	Differences []planDifference
}

func (err *planComparisonError) Error() string {
	first := err.Differences[0]
	return fmt.Sprintf("the migration changes the original managed plan at %s %s in %s (%s)", first.Kind, first.Address, first.Workspace, first.Classification)
}

type comparisonIdentity struct{ kind, workspace, address string }

func planDifferences(original, cutover []planEntryProof) []planDifference {
	before, after := planProofIndex(original), planProofIndex(cutover)
	keys := make(map[comparisonIdentity]bool, len(before)+len(after))
	for key := range before {
		keys[key] = true
	}
	for key := range after {
		keys[key] = true
	}
	differences := []planDifference{}
	for key := range keys {
		oldValue, existed := before[key]
		newValue, exists := after[key]
		if oldValue == newValue {
			continue
		}
		classification := "changed"
		if !existed {
			classification = "added"
		} else if !exists {
			classification = "removed"
		}
		differences = append(differences, planDifference{Kind: key.kind, Workspace: key.workspace, Address: key.address, Classification: classification, Before: oldValue, After: newValue})
	}
	slices.SortFunc(differences, func(left, right planDifference) int {
		if order := cmp.Compare(left.Kind, right.Kind); order != 0 {
			return order
		}
		if order := cmp.Compare(left.Workspace, right.Workspace); order != 0 {
			return order
		}
		return cmp.Compare(left.Address, right.Address)
	})
	return differences
}

func planProofIndex(proof []planEntryProof) map[comparisonIdentity]string {
	index := make(map[comparisonIdentity]string, len(proof))
	for _, entry := range proof {
		index[comparisonIdentity{kind: entry.Kind, workspace: entry.Workspace, address: entry.Address}] = entry.Digest
	}
	return index
}

func writePlanComparison(directory, name, status string, differences []planDifference) error {
	report := struct {
		Version     int              `json:"version"`
		Status      string           `json:"status"`
		Differences []planDifference `json:"differences"`
	}{Version: 1, Status: status, Differences: differences}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return errors.New("encode private native plan comparison metadata")
	}
	return writePlanMetadata(filepath.Join(directory, "reports", name+"-comparison.json"), data)
}

func (e Engine) compareAndReport(original, cutover []planEntryProof, name string) error {
	comparison := comparePlanProof(original, cutover)
	var differences []planDifference
	status := "matched"
	var mismatch *planComparisonError
	if errors.As(comparison, &mismatch) {
		differences, status = mismatch.Differences, "different"
	} else if comparison != nil {
		status = "invalid_proof"
	}
	if err := writePlanComparison(e.Directory, name, status, differences); err != nil {
		return err
	}
	if comparison != nil {
		path := filepath.Join(e.Directory, "reports", name+"-comparison.json")
		slog.Error("migration.plan_comparison.refused", "path", path, "err", migrationErrorType(comparison))
		return fmt.Errorf("%w; private comparison metadata: %s", comparison, path)
	}
	return comparison
}
