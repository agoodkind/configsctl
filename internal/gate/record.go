package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
)

const (
	recordName    = "record.json"
	runDirPerm    = 0o700
	recordPerm    = 0o600
	worktreeName  = "configs"
	runTempName   = "tmp"
	unitPrefix    = "configs-run-"
	finishCommand = "gate-finish"
)

// Record is the stored result of one request.
type Record struct {
	Run           string  `json:"run"`
	Controller    string  `json:"controller"`
	Requester     string  `json:"requester"`
	Request       Request `json:"request"`
	Started       string  `json:"started"`
	Ended         string  `json:"ended,omitempty"`
	ExitStatus    *int    `json:"exit_status,omitempty"`
	ServiceResult string  `json:"service_result,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// Runs is the directory with one subdirectory per run.
type Runs struct {
	Dir string
}

func (r Runs) runDir(run string) string { return filepath.Join(r.Dir, run) }

func (r Runs) worktree(run string) string { return filepath.Join(r.runDir(run), worktreeName) }

func (r Runs) tempDir(run string) string { return filepath.Join(r.runDir(run), runTempName) }

// Create makes the run directory and its temporary directory.
func (r Runs) Create(run string) error {
	if err := os.MkdirAll(r.tempDir(run), runDirPerm); err != nil {
		slog.Error("gate.runs.create_failed", "run", run, "err", err)
		return fmt.Errorf("create run directory %s: %w", run, err)
	}
	return nil
}

// Write stores rec atomically.
func (r Runs) Write(rec Record) error {
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		slog.Error("gate.record.encode_failed", "run", rec.Run, "err", err)
		return fmt.Errorf("encode record %s: %w", rec.Run, err)
	}
	path := filepath.Join(r.runDir(rec.Run), recordName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), recordPerm); err != nil {
		slog.Error("gate.record.write_failed", "run", rec.Run, "err", err)
		return fmt.Errorf("write record %s: %w", rec.Run, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Error("gate.record.rename_failed", "run", rec.Run, "err", err)
		return fmt.Errorf("store record %s: %w", rec.Run, err)
	}
	return nil
}

// Read returns the record of run.
func (r Runs) Read(run string) (Record, error) {
	body, err := os.ReadFile(filepath.Join(r.runDir(run), recordName))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("run %s does not exist on this controller", run)
	}
	if err != nil {
		slog.Error("gate.record.read_failed", "run", run, "err", err)
		return Record{}, fmt.Errorf("read record %s: %w", run, err)
	}
	var rec Record
	if err := json.Unmarshal(body, &rec); err != nil {
		slog.Error("gate.record.decode_failed", "run", run, "err", err)
		return Record{}, fmt.Errorf("decode record %s: %w", run, err)
	}
	return rec, nil
}

// List returns every record, oldest first.
func (r Runs) List() ([]Record, error) {
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		slog.Error("gate.runs.list_failed", "dir", r.Dir, "err", err)
		return nil, fmt.Errorf("list runs: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && runPattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	records := make([]Record, 0, len(names))
	for _, name := range names {
		rec, err := r.Read(name)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}

// PlayLog returns the path of the configsctl run log of run, or an empty
// string when configsctl has not created it yet.
func (r Runs) PlayLog(run string) string {
	matches, err := filepath.Glob(filepath.Join(r.tempDir(run), "configs-runs", "*.log"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}
