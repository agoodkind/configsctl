//go:build unix

package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const fifoInstallWait = 20 * time.Second

func TestProvidersInstallRefusesASymlinkedPackageDirectory(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	elsewhere := filepath.Join(fixture.dir, "elsewhere")
	writeFixtureFile(t, filepath.Join(elsewhere, pveguestBinary+"_v"+assignedVersion), "binary-one")
	writeFixtureFile(t, filepath.Join(elsewhere, "extra-entry"), "unrelated")
	if err := os.MkdirAll(filepath.Dir(fixture.packageDir(assignedVersion)), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(elsewhere, fixture.packageDir(assignedVersion)); err != nil {
		t.Fatal(err)
	}

	linkedSnapshot := mirrorSnapshot(t, elsewhere)

	requireFailure(t, fixture.install(t, pin), "the package directory is not a directory")
	if got := mirrorSnapshot(t, elsewhere); got != linkedSnapshot {
		t.Fatalf("the install changed the linked directory:\n%s\nwant\n%s", got, linkedSnapshot)
	}

	info, err := os.Lstat(fixture.packageDir(assignedVersion))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("package directory = %v, %v, want the original symbolic link", info, err)
	}

	requireStagingEmpty(t, fixture)
	requireArchiveStagingEmpty(t, fixture)
}

func TestProvidersInstallRefusesAFIFOAtTheBinaryPath(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	if err := os.MkdirAll(fixture.packageDir(assignedVersion), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := syscall.Mkfifo(fixture.target(assignedVersion), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), fifoInstallWait)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		configsctlBinary,
		"providers",
		"install",
		pin,
		fixture.mirror,
		fixture.archives,
	)
	command.Dir = fixture.dir
	command.Env = fixture.environment()
	result := finishCommand(t, command)
	if ctx.Err() != nil {
		t.Fatalf("the install blocked on the FIFO until the %s limit: %+v", fifoInstallWait, result)
	}

	if result.exitCode <= 0 {
		t.Fatalf("result = %+v, want a nonzero exit status", result)
	}

	requireFailure(t, result, "the target path is not a regular file")
	info, err := os.Lstat(fixture.target(assignedVersion))
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("target = %v, %v, want the original FIFO", info, err)
	}

	requireStagingEmpty(t, fixture)
	requireArchiveStagingEmpty(t, fixture)
}
