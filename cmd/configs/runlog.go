package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"goodkind.io/configs/internal/clock"
	"goodkind.io/configs/internal/redact"
)

// A run log holds the output of a command that talks to a service over time,
// such as a play walking a fleet of hosts or a tofu run walking a provider API.
// That output arrives in pieces over minutes, and it is the record an operator
// needs when something goes wrong. Sending it to stdout puts it at the mercy of
// whatever consumes stdout, which may truncate it, buffer it, or pipe it into
// something that keeps only a tail. A file does not have that problem: the whole
// stream lands on disk as it is produced, and the command prints the path.

// runLogRoot holds one file per run, relative to the repository root the binary
// runs from. It sits under the ignored .make tree.
const runLogRoot = ".make/runs"

// runLogPerm keeps a log readable only by the operator who ran the command. A
// run prints host names, paths, and diffs, and a diffing run prints file
// contents, so the file is not world-readable even though secrets are redacted.
const runLogPerm = 0o600

// runLogAttempts bounds the search for an unused name. The timestamp has
// one-second resolution, so two runs started in the same second want the same
// name; each retry adds a counter rather than overwriting the earlier run.
const runLogAttempts = 100

// runLog is one command's streamed output on disk. Writes pass through a
// redactor, so a secret the child echoes is replaced before it reaches the file.
// The process-wide stdout and stderr redactors never see this stream, because it
// does not go to those descriptors.
type runLog struct {
	path     string
	file     *os.File
	redactor *redact.Writer
}

// openRunLog creates the log file for one run and returns a writer over it. The
// name carries the caller's label and a UTC timestamp, so concurrent and
// repeated runs never share a file and the directory listing sorts
// chronologically.
func openRunLog(label string, secrets []redact.Pattern) (*runLog, error) {
	if err := os.MkdirAll(runLogRoot, 0o755); err != nil {
		slog.Error("create run log dir failed", "dir", runLogRoot, "err", err)
		return nil, fmt.Errorf("create run log dir: %w", err)
	}
	file, err := createRunLogFile(runLogName(label), clock.FileStamp())
	if err != nil {
		return nil, err
	}
	path, err := filepath.Abs(file.Name())
	if err != nil {
		path = file.Name()
	}
	return &runLog{path: path, file: file, redactor: redact.New(file, secrets)}, nil
}

// createRunLogFile creates a new log file, never an existing one. O_EXCL is what
// makes that true: a concurrent run that wins the race keeps its file and this
// one moves to the next name.
func createRunLogFile(stem, stamp string) (*os.File, error) {
	for attempt := 1; attempt <= runLogAttempts; attempt++ {
		name := stem + "-" + stamp + ".log"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%s-%d.log", stem, stamp, attempt)
		}
		file, err := os.OpenFile(filepath.Join(runLogRoot, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, runLogPerm)
		if err == nil {
			return file, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			slog.Error("create run log failed", "name", name, "err", err)
			return nil, fmt.Errorf("create run log: %w", err)
		}
	}
	err := fmt.Errorf("create run log: %d names already taken for %s-%s", runLogAttempts, stem, stamp)
	slog.Error("run log names exhausted", "stem", stem, "stamp", stamp, "err", err)
	return nil, err
}

// Path is the absolute path the operator reads and tails.
func (l *runLog) Path() string { return l.path }

// Announce tells the operator where the output went, before the command starts
// producing it. Nothing else about the run reaches stdout, so this is the only
// pointer they get.
func (l *runLog) Announce(what string) {
	fmt.Fprintf(os.Stdout, "%s output streams to %s\n", what, l.path)
	fmt.Fprintf(os.Stdout, "Follow it with: tail -f %s\n", l.path)
}

// Write redacts and appends. Each call reaches the file as it happens, so a
// reader tailing the path sees the run progress rather than a batch at exit.
func (l *runLog) Write(p []byte) (int, error) {
	n, err := l.redactor.Write(p)
	if err != nil {
		return n, fmt.Errorf("write run log: %w", err)
	}
	return n, nil
}

// Close flushes the redactor's held-back tail, then closes the file.
func (l *runLog) Close() error {
	if err := l.redactor.Close(); err != nil {
		slog.Error("flush run log failed", "path", l.path, "err", err)
		return fmt.Errorf("flush run log: %w", err)
	}
	if err := l.file.Close(); err != nil {
		slog.Error("close run log failed", "path", l.path, "err", err)
		return fmt.Errorf("close run log: %w", err)
	}
	return nil
}

// runLogNameUnsafe matches every character that does not belong in a filename
// built from a command argument, which may be a bare stem or a path.
var runLogNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// runLogName reduces a label to a filename-safe stem, so both `deploy-mwan` and
// `playbooks/deploy-mwan.yml` name the same kind of log.
func runLogName(label string) string {
	stem := filepath.Base(label)
	stem = strings.TrimSuffix(strings.TrimSuffix(stem, ".yml"), ".yaml")
	stem = runLogNameUnsafe.ReplaceAllString(stem, "-")
	stem = strings.Trim(stem, "-.")
	if stem == "" {
		return "run"
	}
	return stem
}
