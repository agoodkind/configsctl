package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"goodkind.io/configsctl/internal/clock"
)

const (
	journalName = "requests.jsonl"
	resultOK    = "ok"
	resultError = "error"
)

type journalEntry struct {
	Time       string          `json:"time"`
	Controller string          `json:"controller"`
	Requester  string          `json:"requester"`
	Session    string          `json:"session,omitempty"`
	Kind       Kind            `json:"kind,omitempty"`
	Run        string          `json:"run,omitempty"`
	Host       string          `json:"host,omitempty"`
	Playbook   string          `json:"playbook,omitempty"`
	Workspace  string          `json:"workspace,omitempty"`
	Action     TofuAction      `json:"action,omitempty"`
	ExtraVars  json.RawMessage `json:"extra_vars,omitempty"`
	Result     string          `json:"result"`
	Error      string          `json:"error,omitempty"`
}

type journal struct {
	file *os.File
}

func (r Runs) openJournal() (journal, error) {
	if err := os.MkdirAll(r.Dir, runDirPerm); err != nil {
		slog.Error("gate.journal.dir_failed", "dir", r.Dir, "err", err)
		return journal{}, fmt.Errorf("create runs directory %s: %w", r.Dir, err)
	}
	path := filepath.Join(r.Dir, journalName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, recordPerm)
	if err != nil {
		slog.Error("gate.journal.open_failed", "path", path, "err", err)
		return journal{}, fmt.Errorf("open request journal %s: %w", path, err)
	}
	return journal{file: file}, nil
}

func (j journal) record(entry journalEntry) error {
	writeErr := j.write(entry)
	closeErr := j.file.Close()
	if closeErr != nil {
		slog.Error("gate.journal.close_failed", "path", j.file.Name(), "err", closeErr)
		closeErr = fmt.Errorf("close request journal %s: %w", j.file.Name(), closeErr)
	}
	return errors.Join(writeErr, closeErr)
}

func (j journal) write(entry journalEntry) error {
	line, err := json.Marshal(entry)
	if err != nil {
		slog.Error("gate.journal.encode_failed", "path", j.file.Name(), "err", err)
		return fmt.Errorf("encode request journal line: %w", err)
	}
	if _, err := j.file.Write(append(line, '\n')); err != nil {
		slog.Error("gate.journal.write_failed", "path", j.file.Name(), "err", err)
		return fmt.Errorf("write request journal %s: %w", j.file.Name(), err)
	}
	return nil
}

func (s Server) journalEntry(result outcome) journalEntry {
	entry := journalEntry{
		Time:       clock.Stamp(),
		Controller: s.Controller,
		Requester:  s.Requester,
		Session:    result.stored.Session,
		Kind:       result.stored.Kind,
		Run:        result.run,
		Host:       result.stored.Host,
		Playbook:   result.stored.Playbook,
		Workspace:  result.stored.Workspace,
		Action:     result.stored.Action,
		ExtraVars:  result.stored.ExtraVars,
		Result:     resultOK,
		Error:      "",
	}
	if result.err != nil {
		entry.Result = resultError
		entry.Error = result.err.Error()
	}
	return entry
}
