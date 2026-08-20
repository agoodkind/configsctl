package ansible

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirWithAnsibleDir moves the test into a temp working directory that has the
// `ansible` subdirectory runStreaming sets as the child's working directory.
func chdirWithAnsibleDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ansibleDir), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Chdir(root)
	return root
}

// TestRunStreamingWritesChildOutputToTheGivenWriter pins the contract the deploy
// log depends on: when a writer is supplied the child's stdout and stderr both
// land in it, merged in the order the child wrote them, and nothing is lost.
func TestRunStreamingWritesChildOutputToTheGivenWriter(t *testing.T) {
	chdirWithAnsibleDir(t)
	var got bytes.Buffer
	script := `echo first; echo second >&2; echo third`
	if err := runStreaming("sh", []string{"-c", script}, &got); err != nil {
		t.Fatalf("runStreaming: %v", err)
	}
	want := "first\nsecond\nthird\n"
	if got.String() != want {
		t.Fatalf("child output = %q, want %q", got.String(), want)
	}
}

// TestRunStreamingUnbuffersThePythonChild pins that the child runs with
// PYTHONUNBUFFERED set. Ansible's Display.display writes to sys.stdout without
// flushing, so without this the play's output sits in an 8 KiB block buffer and
// a reader tailing the log sees nothing until the buffer fills or the play ends.
func TestRunStreamingUnbuffersThePythonChild(t *testing.T) {
	chdirWithAnsibleDir(t)
	var got bytes.Buffer
	if err := runStreaming("sh", []string{"-c", `printf %s "$PYTHONUNBUFFERED"`}, &got); err != nil {
		t.Fatalf("runStreaming: %v", err)
	}
	if got.String() != "1" {
		t.Fatalf("PYTHONUNBUFFERED in child = %q, want %q", got.String(), "1")
	}
}

// TestRunStreamingKeepsTheParentEnvironment pins that supplying PYTHONUNBUFFERED
// adds to the inherited environment rather than replacing it, so the ansible
// child still sees the operator's PATH and ANSIBLE_* settings.
func TestRunStreamingKeepsTheParentEnvironment(t *testing.T) {
	chdirWithAnsibleDir(t)
	t.Setenv("CONFIGS_STREAMING_PROBE", "inherited")
	var got bytes.Buffer
	if err := runStreaming("sh", []string{"-c", `printf %s "$CONFIGS_STREAMING_PROBE"`}, &got); err != nil {
		t.Fatalf("runStreaming: %v", err)
	}
	if got.String() != "inherited" {
		t.Fatalf("inherited env var in child = %q, want %q", got.String(), "inherited")
	}
}

// TestRunStreamingReportsAFailingChild pins that a non-zero child exit still
// surfaces as an error naming the command, and that the output written before
// the failure is not dropped.
func TestRunStreamingReportsAFailingChild(t *testing.T) {
	chdirWithAnsibleDir(t)
	var got bytes.Buffer
	err := runStreaming("sh", []string{"-c", `echo partial; exit 3`}, &got)
	if err == nil {
		t.Fatal("runStreaming returned nil for a child that exited 3")
	}
	if !strings.Contains(err.Error(), "sh") {
		t.Fatalf("error = %v, want it to name the command", err)
	}
	if got.String() != "partial\n" {
		t.Fatalf("output before failure = %q, want %q", got.String(), "partial\n")
	}
}
