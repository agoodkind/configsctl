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

// runLogDirName holds one file per run. It sits in the host's temp directory
// rather than the repository, so a run leaves no artifact in the working tree
// and the host's own cleanup reclaims old logs.
const runLogDirName = "configs-runs"

// runLogDirPerm keeps the directory listing private to the operator. The host
// temp directory is shared between users on some systems, so the mode matters
// here in a way it does not for a repository-local path.
const runLogDirPerm = 0o700

// runLogPerm keeps a log readable only by the operator who ran the command. A
// run prints host names, paths, and diffs, and a diffing run prints file
// contents, so the file is not world-readable even though secrets are redacted.
const runLogPerm = 0o600

// runLogRoot returns the directory that holds run logs. It resolves the host
// temp directory on every call rather than at init, so a test that redirects
// TMPDIR gets its own directory and never writes to the operator's.
func runLogRoot() string {
	return filepath.Join(os.TempDir(), runLogDirName)
}

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
	dir      *os.Root
	file     *os.File
	redactor *redact.Writer
}

// openRunLog creates the log file for one run and returns a writer over it. The
// name carries the caller's label and a UTC timestamp, so concurrent and
// repeated runs never share a file and the directory listing sorts
// chronologically.
func openRunLog(label string, secrets []redact.Pattern) (*runLog, error) {
	dir, err := openRunLogDir()
	if err != nil {
		return nil, err
	}
	file, err := createRunLogFile(dir, runLogName(label), clock.FileStamp())
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	// A file opened through a directory handle already reports the full path,
	// built from the directory this run opened.
	path, err := filepath.Abs(file.Name())
	if err != nil {
		path = file.Name()
	}
	return &runLog{path: path, dir: dir, file: file, redactor: redact.New(file, secrets)}, nil
}

// openRunLogDir returns a handle to the run log directory, creating it when it
// is absent. The host temp directory is world-writable on many systems, so a
// predictable name inside it is something another local user can create first.
// Creating the directory outright is the safe case, because it either succeeds
// and we own what we made, or it reports that something is already there.
//
// An existing entry is checked and never adjusted. Adjusting it would mean
// acting on whatever it points at: a symlink planted under this name would send
// a mode change to the symlink's target rather than to a directory of ours.
//
// The returned handle confines every later open to this directory, so a symlink
// planted inside it after the check cannot redirect a write elsewhere.
func openRunLogDir() (*os.Root, error) {
	path := runLogRoot()
	err := os.Mkdir(path, runLogDirPerm)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		slog.Error("create run log dir failed", "dir", path, "err", err)
		return nil, fmt.Errorf("create run log dir: %w", err)
	}
	if err != nil {
		if checkErr := checkRunLogDir(path); checkErr != nil {
			return nil, checkErr
		}
	}
	dir, err := os.OpenRoot(path)
	if err != nil {
		slog.Error("open run log dir failed", "dir", path, "err", err)
		return nil, fmt.Errorf("open run log dir: %w", err)
	}
	return dir, nil
}

// checkRunLogDir refuses any pre-existing entry that is not a private directory
// belonging to this user. Lstat reports a symlink as a symlink rather than as
// the thing it points at, which is what makes a planted link visible here.
func checkRunLogDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		slog.Error("stat run log dir failed", "dir", path, "err", err)
		return fmt.Errorf("stat run log dir: %w", err)
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return runLogDirRefusal(path, "it is a symlink")
	case !info.IsDir():
		return runLogDirRefusal(path, "it is not a directory")
	case info.Mode().Perm() != fs.FileMode(runLogDirPerm):
		return runLogDirRefusal(path, fmt.Sprintf("its mode is %v, not %v", info.Mode().Perm(), fs.FileMode(runLogDirPerm)))
	}
	owned, err := ownedByCurrentUser(info)
	if err != nil {
		slog.Error("run log dir owner unavailable", "dir", path, "err", err)
		return fmt.Errorf("check run log dir owner: %w", err)
	}
	if !owned {
		return runLogDirRefusal(path, "another user owns it")
	}
	return nil
}

// runLogDirRefusal reports why the directory was rejected and what to do about
// it. The operator has to clear the path themselves, because removing something
// another user planted is not this tool's decision to make.
func runLogDirRefusal(path, reason string) error {
	err := fmt.Errorf("refusing to use run log dir %s because %s; remove it and run again", path, reason)
	slog.Error("run log dir refused", "dir", path, "reason", reason, "err", err)
	return err
}

// createRunLogFile creates a new log file, never an existing one. O_EXCL is what
// makes that true: a concurrent run that wins the race keeps its file and this
// one moves to the next name. Opening through the directory handle keeps every
// attempt inside that directory.
func createRunLogFile(dir *os.Root, stem, stamp string) (*os.File, error) {
	for attempt := 1; attempt <= runLogAttempts; attempt++ {
		name := stem + "-" + stamp + ".log"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%s-%d.log", stem, stamp, attempt)
		}
		file, err := dir.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, runLogPerm)
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

// Close flushes the redactor's held-back tail, then closes the file and the
// directory handle. The directory handle closes even when the flush fails, so a
// failed run does not leak a descriptor.
func (l *runLog) Close() error {
	flushErr := l.redactor.Close()
	if flushErr != nil {
		slog.Error("flush run log failed", "path", l.path, "err", flushErr)
	}
	fileErr := l.file.Close()
	if fileErr != nil {
		slog.Error("close run log failed", "path", l.path, "err", fileErr)
	}
	if dirErr := l.dir.Close(); dirErr != nil {
		slog.Error("close run log dir failed", "path", l.path, "err", dirErr)
	}
	if flushErr != nil {
		return fmt.Errorf("flush run log: %w", flushErr)
	}
	if fileErr != nil {
		return fmt.Errorf("close run log: %w", fileErr)
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
