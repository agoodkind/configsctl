package statemigration

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

type planReport struct {
	Version     int                  `json:"version"`
	Workspace   string               `json:"workspace"`
	Status      string               `json:"status"`
	Changes     []planChangeMetadata `json:"managed_changes"`
	Diagnostics []planDiagnostic     `json:"diagnostics"`
}

type planChangeMetadata struct {
	Address string   `json:"address"`
	Actions []string `json:"actions"`
	Paths   []string `json:"changed_attribute_paths"`
	Digest  string   `json:"change_digest"`
}

type planReportTarget struct {
	Directory string
	Name      string
	Workspace string
}

func newPlanReport(workspace string) planReport {
	return planReport{Version: 1, Workspace: workspace, Status: "incomplete", Changes: []planChangeMetadata{}, Diagnostics: []planDiagnostic{}}
}

func (target planReportTarget) path() string {
	return filepath.Join(target.Directory, "reports", target.Name)
}

func (target planReportTarget) Write(report planReport) error {
	slog.Info("migration.plan_metadata.write_started", "path", target.path())
	root, err := privateDirectory(filepath.Dir(target.path()))
	if err != nil {
		return err
	}
	defer root.Close()
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return planReportFailure("encode private plan metadata", err)
	}
	file, err := os.CreateTemp(filepath.Dir(target.path()), "report-next-")
	if err != nil {
		return planReportFailure("create private plan metadata", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return planReportFailure("write private plan metadata", err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return planReportFailure("complete private plan metadata", err)
	}
	if err := root.Rename(filepath.Base(file.Name()), target.Name); err != nil {
		return planReportFailure("replace private plan metadata", err)
	}
	return syncDirectory(filepath.Dir(target.path()))
}

func (report *planReport) inspect(contents []byte) error {
	var document struct {
		Format  string            `json:"format_version"`
		Changes []json.RawMessage `json:"resource_changes"`
	}
	if err := json.Unmarshal(contents, &document); err != nil || document.Format != "1.2" {
		return errors.New("the independent plan requires supported native JSON format 1.2")
	}
	var failures []error
	for _, raw := range document.Changes {
		metadata, changed, err := inspectPlanChange(raw)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if changed {
			report.Changes = append(report.Changes, metadata)
		}
	}
	slices.SortFunc(report.Changes, func(left, right planChangeMetadata) int { return cmp.Compare(left.Address, right.Address) })
	if len(failures) != 0 {
		report.Status = "unsupported_plan"
		slog.Error("migration.plan_metadata.unsupported_changes", "changes", len(failures), "err", migrationErrorType(failures[0]))
		return errors.Join(failures...)
	}
	if len(report.Changes) != 0 {
		report.Status = "managed_changes"
		return fmt.Errorf("the independent plan would change managed resource %s", report.Changes[0].Address)
	}
	report.Status = "passed"
	return nil
}

func inspectPlanChange(raw json.RawMessage) (planChangeMetadata, bool, error) {
	var change struct {
		Address string `json:"address"`
		Mode    string `json:"mode"`
		Change  struct {
			Actions []string        `json:"actions"`
			Before  json.RawMessage `json:"before"`
			After   json.RawMessage `json:"after"`
			Unknown json.RawMessage `json:"after_unknown"`
		} `json:"change"`
	}
	if err := json.Unmarshal(raw, &change); err != nil {
		return emptyPlanChange(), false, errors.New("the independent plan contains an invalid resource change")
	}
	if change.Mode == "data" {
		return emptyPlanChange(), false, nil
	}
	if change.Mode != "managed" || change.Address == "" || len(change.Change.Actions) == 0 {
		return emptyPlanChange(), false, errors.New("the independent plan contains an unsupported resource change")
	}
	for _, action := range change.Change.Actions {
		if !slices.Contains([]string{"no-op", "read", "create", "update", "delete", "forget"}, action) {
			return emptyPlanChange(), false, errors.New("the independent plan contains an unsupported resource action")
		}
	}
	if !slices.ContainsFunc(change.Change.Actions, func(action string) bool { return action != "no-op" && action != "read" }) {
		return emptyPlanChange(), false, nil
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return emptyPlanChange(), false, errors.New("normalize private resource change metadata")
	}
	paths, err := changedAttributePaths(change.Change.Before, change.Change.After, change.Change.Unknown)
	if err != nil {
		return emptyPlanChange(), false, err
	}
	return planChangeMetadata{Address: change.Address, Actions: change.Change.Actions, Paths: paths, Digest: digest(canonical)}, true, nil
}

func emptyPlanChange() planChangeMetadata {
	return planChangeMetadata{Address: "", Actions: nil, Paths: nil, Digest: ""}
}

func planReportFailure(operation string, err error) error {
	slog.Error("migration.plan_metadata.failed", "operation", operation, "err", migrationErrorType(err))
	return fmt.Errorf("%s: %w", operation, err)
}
