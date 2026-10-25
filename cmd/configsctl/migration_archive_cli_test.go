//go:build unix

package main_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/procgroup"
)

const (
	archiveRecordSize = 512
	archiveBlockSize  = 20 * archiveRecordSize
	archiveMaxTrailer = archiveBlockSize - archiveRecordSize
	transferDeadline  = time.Minute
)

func padMigrationCLIArchives(t *testing.T, migration *cliMigration) {
	t.Helper()
	original := originalMigrationTree(*migration)
	migration.manifest.RestoreCommit = padMigrationCLIArchive(t, original, migration.manifest.RestoreCommit)
	migration.manifest.Commit = padMigrationCLIArchive(t, migration.tree, migration.manifest.Commit)
	migration.writeManifest(t)
}

func padMigrationCLIArchive(t *testing.T, tree configsTree, commit string) string {
	t.Helper()
	remaining, total := migrationArchiveTrailer(t, tree, commit)
	contentEnd := total - remaining - 2*archiveRecordSize
	padding := (archiveMaxTrailer - contentEnd - archiveRecordSize) % archiveBlockSize
	if padding < 0 {
		padding += archiveBlockSize
	}
	writeFixtureFile(t, filepath.Join(tree.root, "archive-padding.txt"), strings.Repeat("p", padding))
	commit = commitMigrationFixture(t, tree, "archive-padding.txt")
	remaining, _ = migrationArchiveTrailer(t, tree, commit)
	if remaining != archiveMaxTrailer {
		t.Fatalf("the real Git archive retained %d padding bytes; require %d", remaining, archiveMaxTrailer)
	}
	return commit
}

func migrationArchiveTrailer(t *testing.T, tree configsTree, commit string) (int, int) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", "archive", "--format=tar", commit)
	command.Dir = tree.root
	contents, err := command.Output()
	if err != nil {
		t.Fatalf("read the real Git archive for padding setup: %v", err)
	}
	remaining := bytes.NewReader(contents)
	archive := tar.NewReader(remaining)
	for {
		_, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read the real Git archive entry for padding setup: %v", err)
		}
	}
	return remaining.Len(), len(contents)
}

func runBoundedMigrationTransfer(t *testing.T, migration cliMigration, args ...string) commandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), transferDeadline)
	defer cancel()
	command := exec.CommandContext(ctx, configsctlBinary, args...)
	command.Dir = migration.tree.root
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + migration.tree.home,
		"TMPDIR=" + filepath.Join(migration.tree.root, "tmp"),
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := procgroup.Run(command)
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	requireMigrationPrivateOutput(t, result)
	if ctx.Err() != nil {
		t.Fatalf("the public transfer did not complete within %v: %v", transferDeadline, err)
	}
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			t.Fatalf("run the public transfer: %v", err)
		}
		result.exitCode = exited.ExitCode()
	}
	return result
}
