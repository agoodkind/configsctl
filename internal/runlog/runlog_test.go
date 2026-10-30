package runlog_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/runlog"
)

// privateTempDir assigns a test-owned host temp directory. runlog.Root reads
// os.TempDir on every invocation, and os.TempDir reads TMPDIR first. Each test
// uses its own run logs instead of the operator's logs.
func privateTempDir(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

// TestOpenRunLogCreatesAPrivateDirectory checks the mode of a new directory.
// Other users must not read its listing in a shared host temp directory.
func TestOpenRunLogCreatesAPrivateDirectory(t *testing.T) {
	privateTempDir(t)
	log, err := runlog.Open("deploy-mwan", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(runlog.Root())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != fs.FileMode(runlog.DirPerm) {
		t.Fatalf("run log dir mode = %v, want %v", got, fs.FileMode(runlog.DirPerm))
	}
}

// TestOpenRunLogRefusesASymlinkedDirectory checks that Open rejects a symlink.
// Another user could create that symlink before a run. A mode change or log
// write could otherwise affect the symlink target.
func TestOpenRunLogRefusesASymlinkedDirectory(t *testing.T) {
	privateTempDir(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Symlink(victim, runlog.Root()); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	log, err := runlog.Open("deploy-mwan", nil)
	if err == nil {
		_ = log.Close()
		t.Fatal("Open accepted a symlinked run log directory")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error = %v, want it to name the symlink", err)
	}

	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("the symlink target's mode changed to %v, want it untouched at %v", got, fs.FileMode(0o755))
	}
	entries, err := os.ReadDir(victim)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("the symlink target received %d entries, want none", len(entries))
	}
}

// TestOpenRunLogRefusesAWorldReadableDirectory checks that Open rejects an
// existing directory with loose permissions instead of changing its mode.
// Another user could control that path.
func TestOpenRunLogRefusesAWorldReadableDirectory(t *testing.T) {
	privateTempDir(t)
	root := runlog.Root()
	if err := os.Mkdir(root, 0o777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	log, err := runlog.Open("deploy-mwan", nil)
	if err == nil {
		_ = log.Close()
		t.Fatal("Open accepted a world-readable run log directory")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Fatalf("error = %v, want it to name the mode", err)
	}
}

// TestOpenRunLogRefusesAFileUnderTheDirectoryName checks that Open rejects a
// file at the directory path and reports the refusal directly.
func TestOpenRunLogRefusesAFileUnderTheDirectoryName(t *testing.T) {
	privateTempDir(t)
	if err := os.WriteFile(runlog.Root(), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	log, err := runlog.Open("deploy-mwan", nil)
	if err == nil {
		_ = log.Close()
		t.Fatal("Open accepted a file as the run log directory")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v, want it to say the path is not a directory", err)
	}
}

// TestOpenRunLogReusesItsOwnDirectory checks that Open accepts a private
// directory that an earlier run created.
func TestOpenRunLogReusesItsOwnDirectory(t *testing.T) {
	privateTempDir(t)
	first, err := runlog.Open("deploy-mwan", nil)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close first: %v", err)
	}

	second, err := runlog.Open("deploy-proxmox", nil)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close second: %v", err)
	}

	entries, err := os.ReadDir(runlog.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("run log dir contains %d files after two runs, want 2", len(entries))
	}
}

// TestCreateRunLogFileStaysInsideTheDirectory checks that runlog.Create uses
// the directory handle and rejects a symlink to a file outside the directory.
func TestCreateRunLogFileStaysInsideTheDirectory(t *testing.T) {
	privateTempDir(t)
	root := runlog.Root()
	if err := os.Mkdir(root, runlog.DirPerm); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const stamp = "20260821T000000Z"
	planted := "deploy-mwan-" + stamp + ".log"
	if err := os.Symlink(outside, filepath.Join(root, planted)); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	dir, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })

	file, err := runlog.Create(dir, "deploy-mwan", stamp)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := file.WriteString("run output\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(body) != "original\n" {
		t.Fatalf("the file outside the directory was written through the symlink: %q", body)
	}
	if filepath.Base(file.Name()) == planted {
		t.Fatalf("Create reused the planted name %q", planted)
	}
}

// TestOpenRunLogPermissions checks that only the operator can read the log.
// A diffing run can write file contents to the log.
func TestOpenRunLogPermissions(t *testing.T) {
	privateTempDir(t)
	log, err := runlog.Open("deploy-mwan", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(log.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != fs.FileMode(runlog.Perm) {
		t.Fatalf("log mode = %v, want %v", got, fs.FileMode(runlog.Perm))
	}
}

// TestCreateRunLogFileNeverOverwrites checks that runs in the same second
// create separate files. The timestamp has one-second resolution, and a
// counter distinguishes files without overwriting earlier output.
func TestCreateRunLogFileNeverOverwrites(t *testing.T) {
	privateTempDir(t)
	if err := os.Mkdir(runlog.Root(), runlog.DirPerm); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	dir, err := os.OpenRoot(runlog.Root())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })

	const stamp = "20260819T151126Z"
	names := map[string]bool{}
	for run := 1; run <= 3; run++ {
		file, err := runlog.Create(dir, "deploy-mwan", stamp)
		if err != nil {
			t.Fatalf("Create run %d: %v", run, err)
		}
		if _, err := file.WriteString("run\n"); err != nil {
			t.Fatalf("write run %d: %v", run, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close run %d: %v", run, err)
		}
		if names[file.Name()] {
			t.Fatalf("run %d reused the name %s", run, file.Name())
		}
		names[file.Name()] = true
	}

	entries, err := os.ReadDir(runlog.Root())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("run log dir contains %d files after 3 runs, want 3", len(entries))
	}
}

// TestRunLogName checks that accepted labels, including stems and paths,
// produce the same filename-safe stem.
func TestRunLogName(t *testing.T) {
	tests := []struct {
		arg  string
		want string
	}{
		{arg: "deploy-mwan", want: "deploy-mwan"},
		{arg: "playbooks/deploy-mwan.yml", want: "deploy-mwan"},
		{arg: "ansible/playbooks/deploy-testbed.yaml", want: "deploy-testbed"},
		{arg: "tofu-plan", want: "tofu-plan"},
		{arg: "deploy mwan", want: "deploy-mwan"},
		{arg: "../../etc/passwd", want: "passwd"},
		{arg: "/", want: "run"},
	}
	for _, tc := range tests {
		t.Run(tc.arg, func(t *testing.T) {
			if got := runlog.Name(tc.arg); got != tc.want {
				t.Fatalf("Name(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}
