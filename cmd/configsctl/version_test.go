package main

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"testing"

	"goodkind.io/configsctl/internal/version"
)

// versionLineFormat is the exact line `configsctl version` prints, with the
// commit and dirty values left to fill in.
const versionLineFormat = `^commit=%s dirty=%s binhash=[0-9a-f]{12}\n$`

// setVersionStamp writes the link-time stamp fields for one test and restores
// them afterwards.
func setVersionStamp(t *testing.T, commit string, dirty string) {
	t.Helper()
	originalCommit := version.Commit
	originalDirty := version.Dirty
	t.Cleanup(func() {
		version.Commit = originalCommit
		version.Dirty = originalDirty
	})
	version.Commit = commit
	version.Dirty = dirty
}

// runVersionCommand dispatches `configsctl version` through run and returns
// what it wrote to stdout. It runs from an empty directory, so run finds no
// vault and installs no redaction.
func runVersionCommand(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	originalStdout := os.Stdout
	os.Stdout = writer
	runErr := run([]string{"version"})
	os.Stdout = originalStdout
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatalf("close stdout pipe: %v", closeErr)
	}
	output, readErr := io.ReadAll(reader)
	if readErr != nil {
		t.Fatalf("read stdout pipe: %v", readErr)
	}
	if runErr != nil {
		t.Fatalf("run(version) = %v, want nil", runErr)
	}
	return string(output)
}

// TestRunVersionPrintsStampedBuild pins that `configsctl version` prints the
// stamped commit, maps the pipeline's boolean dirty stamp to clean or dirty,
// and appends a 12-character binary hash.
func TestRunVersionPrintsStampedBuild(t *testing.T) {
	cases := []struct {
		name      string
		dirty     string
		wantDirty string
	}{
		{name: "clean release stamp", dirty: "false", wantDirty: "clean"},
		{name: "dirty local stamp", dirty: "true", wantDirty: "dirty"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			setVersionStamp(t, "427cc32", testCase.dirty)
			output := runVersionCommand(t)
			pattern := regexp.MustCompile(fmt.Sprintf(versionLineFormat, "427cc32", testCase.wantDirty))
			if !pattern.MatchString(output) {
				t.Fatalf("version output = %q, want match for %s", output, pattern)
			}
		})
	}
}

// TestRunVersionReportsUnknownWhenUnstamped pins that a binary built without
// the link-time stamp reports unknown for the commit and the dirty flag rather
// than an empty field.
func TestRunVersionReportsUnknownWhenUnstamped(t *testing.T) {
	setVersionStamp(t, "", "")
	output := runVersionCommand(t)
	pattern := regexp.MustCompile(fmt.Sprintf(versionLineFormat, "unknown", "unknown"))
	if !pattern.MatchString(output) {
		t.Fatalf("version output = %q, want match for %s", output, pattern)
	}
}
