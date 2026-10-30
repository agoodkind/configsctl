// Package runlog creates private, redacted logs for long-running commands such
// as Ansible plays and OpenTofu runs. It stores logs in configs-runs under the
// host temp directory.
package runlog

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"goodkind.io/configsctl/internal/clock"
	"goodkind.io/configsctl/internal/fileowner"
	"goodkind.io/configsctl/internal/redact"
)

// runLogDirName identifies the directory for command logs under the host temporary
// directory. The host cleanup can remove old logs without changing the repository.
const runLogDirName = "configs-runs"

// DirPerm restricts the directory listing to the operator. Some systems share
// the host temp directory among users.
const DirPerm = 0o700

// Perm restricts log access to the operator. Logs can contain host names,
// paths, diffs, and file contents even after secret redaction.
const Perm = 0o600

// Root returns the run log directory. It resolves the host temp directory on
// every invocation, including after a test changes TMPDIR.
func Root() string {
	return filepath.Join(os.TempDir(), runLogDirName)
}

// runLogAttempts bounds the search for an unused name. The timestamp has
// one-second resolution, so two runs started in the same second want the same
// name; each retry adds a counter rather than overwriting the earlier run.
const runLogAttempts = 100

// Log writes one command's output to disk. Its redactor replaces secrets before
// writing. The process-wide stdout and stderr redactors do not inspect this
// output.
type Log struct {
	path     string
	dir      *os.Root
	file     *os.File
	redactor *redact.Writer
}

// Open creates a log for one run and returns a writer. The file uses the label
// and a UTC timestamp. Concurrent and repeated runs use separate files, and
// the directory listing sorts them chronologically.
func Open(label string, secrets []redact.Pattern) (*Log, error) {
	dir, err := openRunLogDir()
	if err != nil {
		return nil, err
	}
	file, err := Create(dir, Name(label), clock.FileStamp())
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
	return &Log{path: path, dir: dir, file: file, redactor: redact.New(file, secrets)}, nil
}

// openRunLogDir creates the log directory or checks its existing mode and owner.
// It rejects an existing symlink without changing the target's permissions.
// The returned handle restricts later file creation to this directory.
func openRunLogDir() (*os.Root, error) {
	path := Root()
	err := os.Mkdir(path, DirPerm)
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
	case info.Mode().Perm() != fs.FileMode(DirPerm):
		return runLogDirRefusal(path, fmt.Sprintf("its mode is %v, not %v", info.Mode().Perm(), fs.FileMode(DirPerm)))
	}
	owned, err := fileowner.CurrentUser(info)
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

// Create opens a new log file within dir. It uses O_EXCL to avoid overwriting
// an existing file and tries another filename after a collision.
func Create(dir *os.Root, stem, stamp string) (*os.File, error) {
	for attempt := 1; attempt <= runLogAttempts; attempt++ {
		name := stem + "-" + stamp + ".log"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%s-%d.log", stem, stamp, attempt)
		}
		file, err := dir.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, Perm)
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
func (l *Log) Path() string { return l.path }

// Announce prints the log path and a command for reading output during the run.
func (l *Log) Announce(what string) {
	fmt.Fprintf(os.Stdout, "%s output streams to %s\n", what, l.path)
	fmt.Fprintf(os.Stdout, "Follow it with: tail -f %s\n", l.path)
}

// Write replaces secrets and appends output as the command produces it.
func (l *Log) Write(p []byte) (int, error) {
	n, err := l.redactor.Write(p)
	if err != nil {
		return n, fmt.Errorf("write run log: %w", err)
	}
	return n, nil
}

// Close flushes the redactor's held-back tail, then closes the file and the
// directory handle. The directory handle closes even when the flush fails, so a
// failed run does not leak a descriptor.
func (l *Log) Close() error {
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

// Name converts a label to a filename-safe stem. Both deploy-mwan and
// playbooks/deploy-mwan.yml produce the same stem.
func Name(label string) string {
	stem := filepath.Base(label)
	stem = strings.TrimSuffix(strings.TrimSuffix(stem, ".yml"), ".yaml")
	stem = runLogNameUnsafe.ReplaceAllString(stem, "-")
	stem = strings.Trim(stem, "-.")
	if stem == "" {
		return "run"
	}
	return stem
}
