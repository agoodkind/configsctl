package main_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	pveguestSource    = "tofu.example.test/agoodkind/pveguest"
	pveguestRepo      = "agoodkind/terraform-provider-pveguest"
	pveguestBinary    = "terraform-provider-pveguest"
	assignedVersion   = "0.1.1"
	raceRounds        = 5
	otherPlatform     = "plan9_riscv64"
	watchInterval     = 10 * time.Millisecond
	zeroDigest        = "0000000000000000000000000000000000000000000000000000000000000000"
	lockRootProviders = `terraform {
  required_providers {
    pveguest = {
      source  = "tofu.example.test/agoodkind/pveguest"
      version = ">= 0.1.0"
    }
  }
}
`
)

func hostPlatformName() string {
	return runtime.GOOS + "_" + runtime.GOARCH
}

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func tarGzip(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(zipped)

	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		header := &tar.Header{
			Name:     entry.name,
			Typeflag: typeflag,
			Linkname: entry.linkname,
			Mode:     0o755,
			Size:     int64(len(entry.body)),
		}
		if typeflag != tar.TypeReg {
			header.Size = 0
		}

		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}

		if typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// providerRelease is one pinned provider archive built from fake binary content.
type providerRelease struct {
	tag     string
	version string
	archive []byte
}

func newRelease(t *testing.T, tag, version, content string) providerRelease {
	t.Helper()

	return providerRelease{
		tag:     tag,
		version: version,
		archive: tarGzip(t, tarEntry{name: pveguestBinary, body: content}),
	}
}

func (r providerRelease) asset(platform string) string {
	return pveguestBinary + "_" + platform + ".tar.gz"
}

func (r providerRelease) pinLine(platform string) string {
	return strings.Join(
		[]string{
			pveguestSource,
			pveguestRepo,
			r.tag,
			r.version,
			platform,
			digestOf(r.archive),
		},
		" ",
	)
}

type installFixture struct {
	dir      string
	mirror   string
	archives string
	// path replaces PATH when it is not empty.
	path string
	// extraEnvironment adds variables to the command environment.
	extraEnvironment []string
}

func newInstallFixture(t *testing.T) installFixture {
	t.Helper()
	dir := t.TempDir()

	return installFixture{
		dir:      dir,
		mirror:   filepath.Join(dir, "plugins"),
		archives: filepath.Join(dir, "releases"),
	}
}

func (f installFixture) pin(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(f.dir, name)
	writeFixtureFile(
		t,
		path,
		"# source repository tag version platform sha256\n"+strings.Join(lines, "\n")+"\n",
	)

	return path
}

// cacheArchive places the archive where a previous download would have saved it.
func (f installFixture) cacheArchive(t *testing.T, release providerRelease) {
	t.Helper()
	path := f.archivePath(release)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, release.archive, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f installFixture) archivePath(release providerRelease) string {
	return filepath.Join(f.archives, pveguestBinary, release.tag, release.asset(hostPlatformName()))
}

func (f installFixture) environment() []string {
	path := f.path
	if path == "" {
		path = os.Getenv("PATH")
	}

	environment := []string{"PATH=" + path, "HOME=" + f.dir, "TMPDIR=" + f.dir}

	return append(environment, f.extraEnvironment...)
}

func (f installFixture) command(args ...string) *exec.Cmd {
	command := exec.CommandContext(context.Background(), configsctlBinary, args...)
	command.Dir = f.dir
	command.Env = f.environment()

	return command
}

func (f installFixture) installCommand(pin string) *exec.Cmd {
	return f.command("providers", "install", pin, f.mirror, f.archives)
}

func (f installFixture) install(t *testing.T, pin string) commandResult {
	t.Helper()
	return finishCommand(t, f.installCommand(pin))
}

func finishCommand(t *testing.T, command *exec.Cmd) commandResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()

	return commandOutcome(t, runErr, &stdout, &stderr)
}

// runConcurrently starts every command at once and returns their results in order.
func runConcurrently(t *testing.T, commands ...*exec.Cmd) []commandResult {
	t.Helper()
	stdouts := make([]bytes.Buffer, len(commands))
	stderrs := make([]bytes.Buffer, len(commands))
	runErrors := make([]error, len(commands))
	var group sync.WaitGroup

	for index, command := range commands {
		command.Stdout, command.Stderr = &stdouts[index], &stderrs[index]
		group.Add(1)
		go func() {
			defer group.Done()
			runErrors[index] = command.Run()
		}()
	}

	group.Wait()

	results := make([]commandResult, len(commands))
	for index := range commands {
		results[index] = commandOutcome(t, runErrors[index], &stdouts[index], &stderrs[index])
	}

	return results
}

func commandOutcome(t *testing.T, runErr error, stdout, stderr *bytes.Buffer) commandResult {
	t.Helper()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		result.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run configsctl: %v", runErr)
	}

	return result
}

func (f installFixture) packageDir(version string) string {
	return filepath.Join(f.mirror, filepath.FromSlash(pveguestSource), version, hostPlatformName())
}

func (f installFixture) target(version string) string {
	return filepath.Join(f.packageDir(version), pveguestBinary+"_v"+version)
}

func (f installFixture) stagingRoot() string {
	return filepath.Join(f.dir, ".plugins.staging")
}

// mirrorSnapshot records every entry's path, mode, and content digest.
func mirrorSnapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			info, err := entry.Info()
			if err != nil {
				return err
			}

			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}

			line := fmt.Sprintf("%s %v", relative, info.Mode())
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}

				line += " " + digestOf(data)
			}

			lines = append(lines, line)

			return nil
		},
	)
	if errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Join(lines, "\n")
}

func requireFailure(t *testing.T, result commandResult, want string) {
	t.Helper()
	if result.exitCode == 0 || !strings.Contains(result.stderr, want) {
		t.Fatalf("result = %+v, want a failure containing %q", result, want)
	}
}

func requirePackageHoldsOnlyTheBinary(t *testing.T, f installFixture, version string) {
	t.Helper()
	entries, err := os.ReadDir(f.packageDir(version))
	if err != nil || len(entries) != 1 {
		t.Fatalf("package directory = %v, %v, want only the binary", entries, err)
	}
}

func requireStagingEmpty(t *testing.T, f installFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.stagingRoot())
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		t.Fatalf("staging root keeps %s", entry.Name())
	}
}

func (f installFixture) archiveStagingRoot() string {
	return filepath.Join(f.archives, ".staging")
}

func requireArchiveStagingEmpty(t *testing.T, f installFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.archiveStagingRoot())
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		t.Fatalf("archive staging root keeps %s", entry.Name())
	}
}

func TestProvidersInstallRefusesAnArchiveWithACorruptEnd(t *testing.T) {
	valid := newRelease(t, "tag-one", assignedVersion, "binary-one")
	damagedTrailer := append([]byte(nil), valid.archive...)
	// The last eight bytes of a gzip stream are the CRC-32 and the length.
	damagedTrailer[len(damagedTrailer)-5] ^= 0xff

	cases := map[string][]byte{
		"damaged gzip trailer": damagedTrailer,
		"data after the gzip end": append(
			append([]byte(nil), valid.archive...),
			[]byte("not a gzip stream")...,
		),
		"truncated gzip trailer": valid.archive[:len(valid.archive)-4],
	}
	for name, archive := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newInstallFixture(t)
				release := providerRelease{
					tag:     "tag-one",
					version: assignedVersion,
					archive: archive,
				}
				fixture.cacheArchive(t, release)
				pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
				result := fixture.install(t, pin)
				if result.exitCode == 0 {
					t.Fatalf("install accepted a corrupt archive: %+v", result)
				}

				requireNoFile(t, fixture.target(assignedVersion))
				if got := mirrorSnapshot(t, fixture.mirror); strings.Contains(got, pveguestBinary) {
					t.Fatalf("mirror contains a binary from a corrupt archive:\n%s", got)
				}

				requireStagingEmpty(t, fixture)
				requireArchiveStagingEmpty(t, fixture)
			},
		)
	}
}

func TestProvidersInstallPublishesACachedArchiveWithoutTheVault(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	skipped := newRelease(t, "tag-skipped", "9.9.9", "binary-skipped")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(
		t,
		"providers.pin",
		release.pinLine(hostPlatformName()),
		skipped.pinLine(otherPlatform),
	)
	// An unreadable vault makes every command that loads secrets fail.
	writeFixtureFile(
		t,
		filepath.Join(fixture.dir, "ansible", "inventory", "group_vars", "all", "vault.yml"),
		"not a vault\n",
	)
	writeFixtureFile(
		t,
		filepath.Join(fixture.dir, ".config", "ansible", "vault.pass"),
		"fixture-phrase\n",
	)
	if control := finishCommand(t, fixture.command("version")); control.exitCode == 0 {
		t.Fatal("control: configsctl version succeeded with an unreadable vault")
	}

	result := fixture.install(t, pin)
	requireSuccess(t, result, "providers", "install")
	if !strings.Contains(result.stdout, "installed one provider") {
		t.Fatalf("stdout = %q, want the installed line", result.stdout)
	}

	info, err := os.Stat(fixture.target(assignedVersion))
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o755 {
		t.Fatalf("binary mode = %v, want 0755", info.Mode().Perm())
	}

	content, err := os.ReadFile(fixture.target(assignedVersion))
	if err != nil || string(content) != "binary-one" {
		t.Fatalf("binary content = %q, %v, want the archive member", content, err)
	}

	requirePackageHoldsOnlyTheBinary(t, fixture, assignedVersion)
	requireStagingEmpty(t, fixture)
}

func TestProvidersInstallRefusesACachedArchiveWithAnotherDigest(t *testing.T) {
	fixture := newInstallFixture(t)
	cached := newRelease(t, "tag-one", assignedVersion, "cached")
	pinned := newRelease(t, "tag-one", assignedVersion, "pinned")
	fixture.cacheArchive(t, cached)
	pin := fixture.pin(t, "providers.pin", pinned.pinLine(hostPlatformName()))

	requireFailure(t, fixture.install(t, pin), "archive SHA-256 differs from pinned value")
	if got := mirrorSnapshot(t, fixture.mirror); got != "absent" {
		t.Fatalf("mirror after a digest mismatch = %q, want absent", got)
	}
}

func TestProvidersInstallReusesAnIdenticalBinaryWithoutRewriting(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	requireSuccess(t, fixture.install(t, pin), "providers", "install")
	before, err := os.Stat(fixture.target(assignedVersion))
	if err != nil {
		t.Fatal(err)
	}

	snapshot := mirrorSnapshot(t, fixture.mirror)

	second := fixture.install(t, pin)
	requireSuccess(t, second, "providers", "install")
	if !strings.Contains(second.stdout, "reused one provider") {
		t.Fatalf("stdout = %q, want the reused line", second.stdout)
	}

	after, err := os.Stat(fixture.target(assignedVersion))
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the second install rewrote the installed binary")
	}

	if got := mirrorSnapshot(t, fixture.mirror); got != snapshot {
		t.Fatalf("the second install changed the mirror:\n%s\nwant\n%s", got, snapshot)
	}
}

func TestProvidersInstallRefusesADifferentBinaryAtAnInstalledVersion(t *testing.T) {
	fixture := newInstallFixture(t)
	first := newRelease(t, "tag-one", assignedVersion, "binary-one")
	second := newRelease(t, "tag-two", assignedVersion, "binary-two")
	fixture.cacheArchive(t, first)
	fixture.cacheArchive(t, second)
	firstPin := fixture.pin(t, "first.pin", first.pinLine(hostPlatformName()))
	secondPin := fixture.pin(t, "second.pin", second.pinLine(hostPlatformName()))
	requireSuccess(t, fixture.install(t, firstPin), "providers", "install")
	snapshot := mirrorSnapshot(t, fixture.mirror)

	result := fixture.install(t, secondPin)
	requireFailure(t, result, "a different binary exists at the target")
	requireFailure(t, result, "target_path="+fixture.target(assignedVersion))
	if got := mirrorSnapshot(t, fixture.mirror); got != snapshot {
		t.Fatalf("the refused install changed the mirror:\n%s\nwant\n%s", got, snapshot)
	}

	requireStagingEmpty(t, fixture)
}

func TestProvidersInstallRefusesExtraPackageEntries(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	requireSuccess(t, fixture.install(t, pin), "providers", "install")
	leftover := filepath.Join(
		fixture.packageDir(assignedVersion),
		".terraform-provider-pveguest_v0.1.1.Y0VvPz",
	)
	writeFixtureFile(t, leftover, "leftover")
	snapshot := mirrorSnapshot(t, fixture.mirror)

	result := fixture.install(t, pin)
	requireFailure(t, result, "the package directory contains extra entries")
	requireFailure(t, result, leftover)
	if got := mirrorSnapshot(t, fixture.mirror); got != snapshot {
		t.Fatalf("the refused install changed the mirror:\n%s\nwant\n%s", got, snapshot)
	}
}

func TestProvidersInstallRefusesADirectoryAtTheBinaryPath(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	if err := os.MkdirAll(fixture.target(assignedVersion), 0o755); err != nil {
		t.Fatal(err)
	}

	requireFailure(t, fixture.install(t, pin), "the target path is a directory")
}

func TestProvidersInstallRefusesInvalidPinFiles(t *testing.T) {
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	host := hostPlatformName()

	cases := map[string]struct {
		lines []string
		// A valid row ahead of the bad row must not install.
		leadingValidRow bool
		want            string
	}{
		"extra field": {
			lines:           []string{release.pinLine(host) + " extra"},
			leadingValidRow: true,
			want:            "pin row is invalid",
		},
		"missing digest": {
			lines:           []string{strings.Join(strings.Fields(release.pinLine(host))[:5], " ")},
			leadingValidRow: true,
			want:            "pin row is invalid",
		},
		"uppercase digest": {
			lines:           []string{strings.ToUpper(release.pinLine(host))},
			leadingValidRow: true,
			want:            "pin row is invalid",
		},
		"path traversal": {
			lines:           []string{strings.Replace(release.pinLine(host), "tag-one", "..", 1)},
			leadingValidRow: true,
			want:            "pin row is invalid",
		},
		"no rows": {want: "pin file has no provider row"},
		"no host platform row": {
			lines: []string{release.pinLine(otherPlatform)},
			want:  "no row for the provider on the host platform",
		},
	}
	for name, tc := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newInstallFixture(t)
				fixture.cacheArchive(t, release)
				var lines []string
				if tc.leadingValidRow {
					lines = append(lines, release.pinLine(host))
				}

				lines = append(lines, tc.lines...)
				requireFailure(
					t,
					fixture.install(t, fixture.pin(t, "providers.pin", lines...)),
					tc.want,
				)
				if got := mirrorSnapshot(t, fixture.mirror); got != "absent" {
					t.Fatalf("mirror after an invalid pin file = %q, want absent", got)
				}
			},
		)
	}
}

func TestProvidersInstallRejectsUnsafeArchiveMembers(t *testing.T) {
	cases := map[string][]tarEntry{
		"symlink member": {
			{name: pveguestBinary, typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
		},
		"missing member": {{name: "other-file", body: "x"}},
	}
	for name, entries := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newInstallFixture(t)
				release := providerRelease{
					tag:     "tag-one",
					version: assignedVersion,
					archive: tarGzip(t, entries...),
				}
				fixture.cacheArchive(t, release)
				pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
				result := fixture.install(t, pin)
				if result.exitCode == 0 {
					t.Fatalf("install accepted an unsafe archive: %+v", result)
				}

				if got := mirrorSnapshot(t, fixture.mirror); strings.Contains(got, pveguestBinary) {
					t.Fatalf("mirror contains a binary from an unsafe archive:\n%s", got)
				}
			},
		)
	}

	t.Run(
		"traversal entry beside the member",
		func(t *testing.T) {
			fixture := newInstallFixture(t)
			archive := tarGzip(
				t,
				tarEntry{name: "../escaped", body: "escaped"},
				tarEntry{name: "/absolute-escape", body: "escaped"},
				tarEntry{name: pveguestBinary, body: "binary-one"},
			)
			release := providerRelease{tag: "tag-one", version: assignedVersion, archive: archive}
			fixture.cacheArchive(t, release)
			pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
			requireSuccess(t, fixture.install(t, pin), "providers", "install")
			requireNoFile(t, filepath.Join(filepath.Dir(fixture.dir), "escaped"))
			requireNoFile(t, "/absolute-escape")
		},
	)
}

func TestProvidersInstallUsageErrors(t *testing.T) {
	fixture := newInstallFixture(t)

	for _, args := range [][]string{
		{"providers"},
		{"providers", "bogus"},
		{"providers", "install", "one"},
		{"providers", "install", "a", "b"},
	} {
		requireFailure(
			t,
			finishCommand(t, fixture.command(args...)),
			"usage: configsctl providers install",
		)
	}
}

func TestProvidersInstallUsesTheDefaultPaths(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	cachedPath := filepath.Join(
		fixture.dir,
		".make",
		"releases",
		pveguestBinary,
		"tag-one",
		release.asset(hostPlatformName()),
	)
	if err := os.MkdirAll(filepath.Dir(cachedPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cachedPath, release.archive, 0o644); err != nil {
		t.Fatal(err)
	}

	writeFixtureFile(
		t,
		filepath.Join(fixture.dir, "opentofu", "guest", "providers.pin"),
		release.pinLine(hostPlatformName())+"\n",
	)
	requireSuccess(
		t,
		finishCommand(t, fixture.command("providers", "install")),
		"providers",
		"install",
	)
	mirror := filepath.Join(fixture.dir, "opentofu", "terraform.d", "plugins")
	requireFile(
		t,
		filepath.Join(
			mirror,
			filepath.FromSlash(pveguestSource),
			assignedVersion,
			hostPlatformName(),
			pveguestBinary+"_v"+assignedVersion,
		),
	)
}

// Pinned mwan release archives by host platform, copied from the Configs pin file.
var mwanArchiveDigests = map[string]string{
	"darwin_arm64": "6ea96451df5207fe4ea6039d5487b5bdff4c2534673ab5400731ee197f03dade",
	"linux_amd64":  "aeabbf307bf2d28942ff1d21f8231a3fe60b0371d7f6b307b8b4075de4448fde",
	"linux_arm64":  "3f77880c8ca79b179d2e4248924bd07af93565c86a620f9601462f109602c31f",
}

const (
	mwanSource = "tofu.home.arpa/agoodkind/mwan"
	mwanRepo   = "agoodkind/mwan"
	mwanTag    = "202610192152-ef-9bc78a1"
	mwanBinary = "terraform-provider-mwan"
)

func mwanPinLine(digest string) string {
	return strings.Join(
		[]string{mwanSource, mwanRepo, mwanTag, "0.1.0", hostPlatformName(), digest},
		" ",
	)
}

func TestProvidersInstallDownloadsThroughGhOnACacheMiss(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh is not installed: the download through gh cannot run")
	}

	if err := exec.CommandContext(context.Background(), "gh", "auth", "status").Run(); err != nil {
		t.Skipf("gh is not authenticated (%v): the download through gh cannot run", err)
	}

	digest, known := mwanArchiveDigests[hostPlatformName()]
	if !known {
		t.Skipf("the pinned mwan release has no archive for %s", hostPlatformName())
	}

	// gh reads its credentials from the real home directory and environment.
	runWithRealEnvironment := func(fixture installFixture, pin string) commandResult {
		command := fixture.installCommand(pin)
		command.Env = os.Environ()

		return finishCommand(t, command)
	}
	releaseDir := func(fixture installFixture) string {
		return filepath.Join(fixture.archives, mwanBinary, mwanTag)
	}

	t.Run(
		"verified download is published and installed",
		func(t *testing.T) {
			fixture := newInstallFixture(t)
			pin := fixture.pin(t, "providers.pin", mwanPinLine(digest))
			result := runWithRealEnvironment(fixture, pin)
			requireSuccess(t, result, "providers", "install")
			archive := filepath.Join(
				releaseDir(fixture),
				mwanBinary+"_"+hostPlatformName()+".tar.gz",
			)
			data, err := os.ReadFile(archive)
			if err != nil || digestOf(data) != digest {
				t.Fatalf("published archive digest mismatch: %v", err)
			}

			entries, err := os.ReadDir(releaseDir(fixture))
			if err != nil || len(entries) != 1 {
				t.Fatalf("archive cache = %v, %v, want only the published archive", entries, err)
			}

			binary := filepath.Join(
				fixture.mirror,
				filepath.FromSlash(mwanSource),
				"0.1.0",
				hostPlatformName(),
				mwanBinary+"_v0.1.0",
			)
			requireFile(t, binary)
		},
	)
	t.Run(
		"digest mismatch publishes nothing",
		func(t *testing.T) {
			fixture := newInstallFixture(t)
			pin := fixture.pin(t, "providers.pin", mwanPinLine(zeroDigest))
			requireFailure(
				t,
				runWithRealEnvironment(fixture, pin),
				"archive SHA-256 differs from pinned value",
			)
			requireNoFile(t, releaseDir(fixture))
			requireArchiveStagingEmpty(t, fixture)
			if got := mirrorSnapshot(t, fixture.mirror); got != "absent" {
				t.Fatalf("mirror after a failed verification = %q, want absent", got)
			}
		},
	)
}

func tofuCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.CommandContext(
		context.Background(),
		"tofu",
		append([]string{"-chdir=" + dir}, args...)...,
	)
	command.Env = append(os.Environ(), "TF_IN_AUTOMATION=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("tofu %v: %v\n%s", args, err, output)
	}
}

// lockMirror runs tofu providers lock against the mirror and returns the
// lockfile, which records the h1 checksum tofu computes for the package.
func lockMirror(t *testing.T, root, mirror string) string {
	t.Helper()
	writeFixtureFile(t, filepath.Join(root, "main.tf"), lockRootProviders)
	tofuCommand(
		t,
		root,
		"providers",
		"lock",
		"-fs-mirror="+mirror,
		"-platform="+hostPlatformName(),
		pveguestSource,
	)
	lock, err := os.ReadFile(filepath.Join(root, ".terraform.lock.hcl"))
	if err != nil {
		t.Fatal(err)
	}

	return string(lock)
}

func initReadonlyRoot(t *testing.T, root, mirror, lock string) {
	t.Helper()
	writeFixtureFile(t, filepath.Join(root, "main.tf"), lockRootProviders)
	writeFixtureFile(t, filepath.Join(root, ".terraform.lock.hcl"), lock)
	tofuCommand(
		t,
		root,
		"init",
		"-backend=false",
		"-input=false",
		"-lockfile=readonly",
		"-plugin-dir="+mirror,
	)
}

func TestProvidersInstallKeepsTofuProviderLockChecksumsStable(t *testing.T) {
	requireTofu(t)
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	replacement := newRelease(t, "tag-two", assignedVersion, "binary-two")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	requireSuccess(t, fixture.install(t, pin), "providers", "install")
	firstSnapshot := mirrorSnapshot(t, fixture.mirror)
	reference := lockMirror(t, filepath.Join(fixture.dir, "reference-root"), fixture.mirror)

	requireSuccess(t, fixture.install(t, pin), "providers", "install")
	if got := mirrorSnapshot(t, fixture.mirror); got != firstSnapshot {
		t.Fatalf("the second install changed the mirror:\n%s\nwant\n%s", got, firstSnapshot)
	}

	if got := lockMirror(
		t,
		filepath.Join(fixture.dir, "second-root"),
		fixture.mirror,
	); got != reference {
		t.Fatalf("lockfile after the second install differs:\n%s\nwant\n%s", got, reference)
	}

	// Staging leftovers of an exited process stay outside the package checksums.
	staleRun := filepath.Join(fixture.stagingRoot(), "run.2147483646.killed")
	writeFixtureFile(
		t,
		filepath.Join(staleRun, pveguestBinary+"_v"+assignedVersion+".leftover"),
		string(replacement.archive),
	)
	initReadonlyRoot(t, filepath.Join(fixture.dir, "leftover-init-root"), fixture.mirror, reference)
	if got := lockMirror(
		t,
		filepath.Join(fixture.dir, "leftover-lock-root"),
		fixture.mirror,
	); got != reference {
		t.Fatalf("lockfile with staging leftovers differs:\n%s\nwant\n%s", got, reference)
	}

	requireSuccess(t, fixture.install(t, pin), "providers", "install")
	initReadonlyRoot(t, filepath.Join(fixture.dir, "rerun-init-root"), fixture.mirror, reference)
}

func TestProvidersInstallRemovesOnlyStagingOfExitedProcesses(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	exited := exec.CommandContext(context.Background(), "true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}

	staleRun := filepath.Join(
		fixture.stagingRoot(),
		fmt.Sprintf("run.%d.killed", exited.Process.Pid),
	)
	liveRun := filepath.Join(fixture.stagingRoot(), fmt.Sprintf("run.%d.live", os.Getpid()))

	foreign := filepath.Join(fixture.stagingRoot(), "operator-notes")
	for _, directory := range []string{staleRun, liveRun, foreign} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	result := fixture.install(t, pin)
	requireSuccess(t, result, "providers", "install")
	requireNoFile(t, staleRun)
	requireFile(t, liveRun)
	requireFile(t, foreign)
}

func TestProvidersInstallConcurrentProcessesNeverReplaceTheTarget(t *testing.T) {
	first := newRelease(t, "tag-one", assignedVersion, "binary-one")
	second := newRelease(t, "tag-two", assignedVersion, "binary-two")

	for round := 1; round <= raceRounds; round++ {
		t.Run(
			fmt.Sprintf("round %d", round),
			func(t *testing.T) {
				fixture := newInstallFixture(t)
				fixture.cacheArchive(t, first)
				fixture.cacheArchive(t, second)
				firstPin := fixture.pin(t, "first.pin", first.pinLine(hostPlatformName()))
				secondPin := fixture.pin(t, "second.pin", second.pinLine(hostPlatformName()))
				watcher := watchTarget(fixture.target(assignedVersion))

				results := runConcurrently(
					t,
					fixture.installCommand(firstPin),
					fixture.installCommand(secondPin),
				)
				if violation := watcher.stop(); violation != "" {
					t.Fatal(violation)
				}

				winner, loser, content := results[0], results[1], "binary-one"
				if results[0].exitCode != 0 {
					winner, loser, content = results[1], results[0], "binary-two"
				}

				if winner.exitCode != 0 || loser.exitCode == 0 {
					t.Fatalf("want exactly one success, got %+v", results)
				}

				requireFailure(t, loser, "a different binary exists at the target")
				requireFailure(t, loser, "target_path="+fixture.target(assignedVersion))
				data, err := os.ReadFile(fixture.target(assignedVersion))
				if err != nil || string(data) != content {
					t.Fatalf("target = %q, %v, want %q", data, err, content)
				}

				requirePackageHoldsOnlyTheBinary(t, fixture, assignedVersion)
				requireStagingEmpty(t, fixture)
			},
		)
	}
}

func TestProvidersInstallConcurrentProcessesWithTheSamePinBothSucceed(t *testing.T) {
	fixture := newInstallFixture(t)
	release := newRelease(t, "tag-one", assignedVersion, "binary-one")
	fixture.cacheArchive(t, release)
	pin := fixture.pin(t, "providers.pin", release.pinLine(hostPlatformName()))
	watcher := watchTarget(fixture.target(assignedVersion))

	results := runConcurrently(t, fixture.installCommand(pin), fixture.installCommand(pin))
	if violation := watcher.stop(); violation != "" {
		t.Fatal(violation)
	}

	for _, result := range results {
		requireSuccess(t, result, "providers", "install")
	}

	requirePackageHoldsOnlyTheBinary(t, fixture, assignedVersion)
	requireStagingEmpty(t, fixture)
}

// targetWatcher samples the target and reports a change of its file or bytes.
type targetWatcher struct {
	done   chan struct{}
	result chan string
}

func watchTarget(target string) *targetWatcher {
	watcher := &targetWatcher{done: make(chan struct{}), result: make(chan string, 1)}
	go func() {
		var first os.FileInfo
		var firstDigest string
		sample := func() string {
			info, err := os.Stat(target)
			if err != nil {
				return ""
			}

			data, err := os.ReadFile(target)
			if err != nil {
				return ""
			}

			if first == nil {
				first, firstDigest = info, digestOf(data)
				return ""
			}

			if !os.SameFile(first, info) || digestOf(data) != firstDigest {
				return "the target file or its bytes changed after its creation"
			}

			return ""
		}

		for {
			select {
			case <-watcher.done:
				watcher.result <- sample()
				return
			case <-time.After(watchInterval):
				if violation := sample(); violation != "" {
					watcher.result <- violation
					return
				}
			}
		}
	}()

	return watcher
}

func (w *targetWatcher) stop() string {
	close(w.done)
	return <-w.result
}
