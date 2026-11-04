package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"goodkind.io/configsctl/internal/procgroup"
)

const providersUsage = "usage: configsctl providers install [<pin_file> <mirror_dir> <archive_dir>]"

const (
	defaultPinFile        = "opentofu/guest/providers.pin"
	defaultProviderMirror = "opentofu/terraform.d/plugins"
	defaultArchiveDir     = ".make/releases"
	pinnedArgumentCount   = 3
	pinFieldCount         = 6
	maxBinaryBytes        = 1 << 30
	maxTrailingBytes      = 1 << 20
	pidBitSize            = 31
	sharedDirectoryMode   = 0o755
	privateFileMode       = 0o600
	binaryFileMode        = 0o755
	executeBits           = 0o111
	stagingRunPrefix      = "run."
	cacheStagingName      = ".staging"
	partialDirectoryName  = ".partial-*"
)

var (
	pinSHA256Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinRepositoryName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	supportedKernels   = []string{"darwin", "linux"}
	supportedMachines  = []string{"arm64", "amd64"}
	errInstallStopped  = errors.New("provider installation stopped")
	errPinRowMalformed = errors.New("pin row is invalid")
)

// pinRow is one non-comment row of the pin file.
type pinRow struct {
	source     string
	repository string
	tag        string
	version    string
	platform   string
	sha256     string
}

type providersInstallOptions struct {
	pinFile    string
	mirrorDir  string
	archiveDir string
}

// runProviders runs the providers subcommands. providers install reads no
// vault value and does not read the vault password file.
func runProviders(args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return errors.New(providersUsage)
	}
	options := providersInstallOptions{
		pinFile: defaultPinFile, mirrorDir: defaultProviderMirror, archiveDir: defaultArchiveDir,
	}
	if paths := args[1:]; len(paths) == pinnedArgumentCount {
		triple := [pinnedArgumentCount]string(paths)
		options = providersInstallOptions{pinFile: triple[0], mirrorDir: triple[1], archiveDir: triple[2]}
	} else if len(paths) != 0 {
		return errors.New(providersUsage)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return installProviders(ctx, options)
}

// providerInstallError logs a failed operation on subject and wraps its cause.
func providerInstallError(operation, subject string, err error) error {
	slog.Error("providers.install_failed", "operation", operation, "subject", subject, "err", err)
	return fmt.Errorf("%s %s: %w", operation, subject, err)
}

// stoppedError reports that a signal canceled the installation.
func stoppedError(ctx context.Context) error {
	cause := context.Cause(ctx)
	slog.Error("providers.install_stopped", "err", cause)
	return fmt.Errorf("%w: %w", errInstallStopped, cause)
}

func hostProviderPlatform() (string, error) {
	if !slices.Contains(supportedKernels, runtime.GOOS) {
		return "", fmt.Errorf("unsupported host kernel kernel_name=%s", runtime.GOOS)
	}
	if !slices.Contains(supportedMachines, runtime.GOARCH) {
		return "", fmt.Errorf("unsupported host machine type machine_name=%s", runtime.GOARCH)
	}
	return runtime.GOOS + "_" + runtime.GOARCH, nil
}

func installProviders(ctx context.Context, options providersInstallOptions) error {
	platform, err := hostProviderPlatform()
	if err != nil {
		return err
	}
	rows, err := readPinFile(options.pinFile)
	if err != nil {
		return err
	}
	if err := requireHostRows(rows, platform, options.pinFile); err != nil {
		return err
	}
	mirror, err := filepath.Abs(options.mirrorDir)
	if err != nil {
		return providerInstallError("resolve the mirror directory", options.mirrorDir, err)
	}
	archiveDir, err := filepath.Abs(options.archiveDir)
	if err != nil {
		return providerInstallError("resolve the archive directory", options.archiveDir, err)
	}
	// OpenTofu does not read the staging root beside the mirror, so temporary
	// files do not change package checksums.
	stagingRoot := filepath.Join(filepath.Dir(mirror), "."+filepath.Base(mirror)+".staging")
	if err := os.MkdirAll(stagingRoot, sharedDirectoryMode); err != nil {
		return providerInstallError("create the staging root", stagingRoot, err)
	}
	if err := removeStaleStaging(stagingRoot); err != nil {
		return err
	}
	runDir, err := createRunDirectory(stagingRoot)
	if err != nil {
		return err
	}
	defer removeRunDirectory(runDir)
	// Downloads stay on the archive cache filesystem to allow an atomic rename
	// into the cache. The cache staging root has the same process ownership scan.
	cacheStagingRoot := filepath.Join(archiveDir, cacheStagingName)
	if err := os.MkdirAll(cacheStagingRoot, sharedDirectoryMode); err != nil {
		return providerInstallError("create the archive staging root", cacheStagingRoot, err)
	}
	if err := removeStaleStaging(cacheStagingRoot); err != nil {
		return err
	}
	cacheRunDir, err := createRunDirectory(cacheStagingRoot)
	if err != nil {
		return err
	}
	defer removeRunDirectory(cacheRunDir)
	for _, row := range rows {
		if row.platform != platform {
			continue
		}
		if ctx.Err() != nil {
			return stoppedError(ctx)
		}
		stagingDirs := stagingDirectories{binaries: runDir, downloads: cacheRunDir}
		if err := installProvider(ctx, row, mirror, archiveDir, stagingDirs); err != nil {
			return err
		}
	}
	if _, err := providerMirrorSources(mirror); err != nil {
		return err
	}
	return nil
}

// readPinFile parses every row before any installation starts.
func readPinFile(pinFile string) ([]pinRow, error) {
	content, err := os.ReadFile(pinFile)
	if err != nil {
		return nil, providerInstallError("read the pin file", pinFile, err)
	}
	var rows []pinRow
	for index, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		row, err := parsePinRow(fields)
		if err != nil {
			slog.Error("providers.pin_row_invalid", "pin_file", pinFile, "line_number", index+1, "err", err)
			return nil, fmt.Errorf("%w pin_file=%s line_number=%d: %w", errPinRowMalformed, pinFile, index+1, err)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("pin file has no provider row pin_file=%s", pinFile)
	}
	return rows, nil
}

func parsePinRow(fields []string) (pinRow, error) {
	if len(fields) != pinFieldCount || !pinSHA256Pattern.MatchString(fields[pinFieldCount-1]) {
		return pinRow{}, errors.New("the row must have six fields ending in a lowercase SHA-256 digest")
	}
	row := pinRow{
		source: fields[0], repository: fields[1], tag: fields[2],
		version: fields[3], platform: fields[4], sha256: fields[5],
	}
	// These values become path segments.
	segments := strings.Split(row.source, "/")
	const sourceParts = 3
	if len(segments) != sourceParts {
		return pinRow{}, fmt.Errorf("source address %q must have three path components", row.source)
	}
	for _, value := range append(segments, row.tag, row.version, row.platform) {
		if !providerDirectoryName.MatchString(value) {
			return pinRow{}, fmt.Errorf("value %q contains characters that a path segment cannot hold", value)
		}
	}
	if !pinRepositoryName.MatchString(row.repository) {
		return pinRow{}, fmt.Errorf("repository %q must be owner/name", row.repository)
	}
	return row, nil
}

// requireHostRows refuses a provider that has no row for the host platform.
func requireHostRows(rows []pinRow, platform, pinFile string) error {
	var sources, installed []string
	for _, row := range rows {
		if !slices.Contains(sources, row.source) {
			sources = append(sources, row.source)
		}
		if row.platform == platform && !slices.Contains(installed, row.source) {
			installed = append(installed, row.source)
		}
	}
	for _, source := range sources {
		if !slices.Contains(installed, source) {
			return fmt.Errorf("pin file has no row for the provider on the host platform pin_file=%s source_address=%s platform=%s", pinFile, source, platform)
		}
	}
	return nil
}

// removeStaleStaging deletes run.<pid>.* directories of exited processes. A
// SIGKILL, an out-of-memory kill, or a power loss skips the normal cleanup.
// Directories of running processes and other entries stay.
func removeStaleStaging(stagingRoot string) error {
	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		return providerInstallError("read the staging root", stagingRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), stagingRunPrefix) {
			continue
		}
		pidText, _, _ := strings.Cut(strings.TrimPrefix(entry.Name(), stagingRunPrefix), ".")
		pid, parseErr := strconv.ParseUint(pidText, 10, pidBitSize)
		if parseErr != nil || pid == 0 {
			continue
		}
		running, err := processRunning(int(pid))
		if err != nil {
			return providerInstallError("check the process of the staging directory", entry.Name(), err)
		}
		if running {
			continue
		}
		runDir := filepath.Join(stagingRoot, entry.Name())
		if err := os.RemoveAll(runDir); err != nil {
			return providerInstallError("remove the stale staging directory", runDir, err)
		}
		slog.Debug("providers.stale_staging_removed", "run_dir", runDir, "run_pid", pid)
		fmt.Printf("Removed staging directory for an exited process run_dir=%s run_pid=%d\n", runDir, pid)
	}
	return nil
}

// stagingDirectories are the per-run directories that the exited-process scan
// reclaims after a killed run.
type stagingDirectories struct {
	binaries  string
	downloads string
}

// createRunDirectory creates a run.<pid>.* directory with mode 0700.
func createRunDirectory(stagingRoot string) (string, error) {
	runDir, err := os.MkdirTemp(stagingRoot, stagingRunPrefix+strconv.Itoa(os.Getpid())+".*")
	if err != nil {
		return "", providerInstallError("create the run staging directory in", stagingRoot, err)
	}
	return runDir, nil
}

func removeRunDirectory(runDir string) {
	if err := os.RemoveAll(runDir); err != nil {
		slog.Error("providers.staging_cleanup_failed", "run_dir", runDir, "err", err)
	}
}

func installProvider(ctx context.Context, row pinRow, mirror, archiveDir string, staging stagingDirectories) error {
	runDir := staging.binaries
	binaryName := "terraform-provider-" + path.Base(row.source)
	assetName := binaryName + "_" + row.platform + ".tar.gz"
	releaseDir := filepath.Join(archiveDir, binaryName, row.tag)
	archivePath := filepath.Join(releaseDir, assetName)
	targetDir := filepath.Join(mirror, filepath.FromSlash(row.source), row.version, row.platform)
	targetPath := filepath.Join(targetDir, binaryName+"_v"+row.version)

	if err := ensureArchive(ctx, row, assetName, staging.downloads, archivePath); err != nil {
		return err
	}
	if err := checkArchiveDigest(archivePath, row.sha256); err != nil {
		return err
	}
	if err := checkPackageDirectory(targetDir, targetPath); err != nil {
		return err
	}
	// OpenTofu links initialized roots to the mirror. Replacing a binary changes
	// the executable those roots run and breaks their lockfile checksums.
	candidate, memberDigest, err := extractCandidate(archivePath, binaryName, row.version, runDir)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return stoppedError(ctx)
	}
	if err := os.MkdirAll(targetDir, sharedDirectoryMode); err != nil {
		return providerInstallError("create the package directory", targetDir, err)
	}
	action, err := publishCandidate(candidate, targetPath, row)
	if err != nil {
		return err
	}
	if err := os.Remove(candidate); err != nil {
		return providerInstallError("remove the candidate", candidate, err)
	}
	if err := checkInstalledBinary(targetPath, memberDigest); err != nil {
		return err
	}
	slog.Info("providers.published", "action", action, "source_address", row.source, "tag", row.tag, "target_path", targetPath)
	fmt.Printf("%s one provider source_address=%s tag=%s target_path=%s\n", action, row.source, row.tag, targetPath)
	return nil
}

// ensureArchive downloads the release asset when the archive cache lacks it.
func ensureArchive(ctx context.Context, row pinRow, assetName, downloadDir, archivePath string) error {
	info, err := os.Stat(archivePath)
	switch {
	case err == nil && info.Mode().IsRegular():
		return nil
	case err == nil:
		return fmt.Errorf("the cached archive is not a regular file archive_path=%s", archivePath)
	case !errors.Is(err, fs.ErrNotExist):
		return providerInstallError("inspect the cached archive", archivePath, err)
	}
	return downloadArchive(ctx, row, assetName, downloadDir, archivePath)
}

// downloadArchive runs gh release download into a private partial directory
// inside the run's download staging directory and publishes the archive to the
// cache only after its digest matches the pin. A failed or interrupted
// download leaves nothing at the published path, and the exited-process scan
// reclaims the partial directory of a killed run.
func downloadArchive(ctx context.Context, row pinRow, assetName, downloadDir, archivePath string) error {
	partialDir, err := os.MkdirTemp(downloadDir, partialDirectoryName)
	if err != nil {
		return providerInstallError("create the partial download directory in", downloadDir, err)
	}
	defer func() {
		if removeErr := os.RemoveAll(partialDir); removeErr != nil {
			slog.Error("providers.partial_cleanup_failed", "partial_dir", partialDir, "err", removeErr)
		}
	}()
	downloadArgs, err := sanitizeTofuArgs([]string{
		"release", "download", row.tag, "--repo", row.repository, "--pattern", assetName, "--dir", partialDir,
	})
	if err != nil {
		return providerInstallError("validate the gh arguments for", assetName, err)
	}
	command := exec.CommandContext(ctx, "gh", downloadArgs...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := procgroup.Run(command); err != nil {
		if ctx.Err() != nil {
			return stoppedError(ctx)
		}
		return providerInstallError("download the release archive with gh for", row.repository+" "+row.tag+" "+assetName, err)
	}
	partialPath := filepath.Join(partialDir, assetName)
	info, err := os.Lstat(partialPath)
	if err != nil {
		return providerInstallError("inspect the downloaded archive", partialPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the downloaded archive is not a regular file partial_path=%s", partialPath)
	}
	if err := os.Chmod(partialPath, privateFileMode); err != nil {
		return providerInstallError("set the mode of the downloaded archive", partialPath, err)
	}
	if err := checkArchiveDigest(partialPath, row.sha256); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(archivePath), sharedDirectoryMode); err != nil {
		return providerInstallError("create the archive directory", filepath.Dir(archivePath), err)
	}
	if err := os.Rename(partialPath, archivePath); err != nil {
		return providerInstallError("publish the downloaded archive to", archivePath, err)
	}
	return nil
}

func digestMismatch(archivePath, expected, actual string) error {
	return fmt.Errorf("archive SHA-256 differs from pinned value archive_path=%s expected_sha256=%s actual_sha256=%s", archivePath, expected, actual)
}

func fileSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", providerInstallError("open for hashing", filePath, err)
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", providerInstallError("hash", filePath, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func checkArchiveDigest(archivePath, expected string) error {
	actual, err := fileSHA256(archivePath)
	if err != nil {
		return err
	}
	if actual != expected {
		return digestMismatch(archivePath, expected, actual)
	}
	return nil
}

// checkPackageDirectory refuses a directory at the binary path and any entry
// beside the binary. OpenTofu includes every entry of a package directory in
// its lockfile checksum.
func checkPackageDirectory(targetDir, targetPath string) error {
	info, err := os.Lstat(targetPath)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("cannot publish the binary because the target path is a directory target_path=%s", targetPath)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return providerInstallError("inspect the target path", targetPath, err)
	}
	var extras []string
	walkErr := filepath.WalkDir(targetDir, func(entryPath string, _ fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && entryPath == targetDir {
				return filepath.SkipAll
			}
			return err
		}
		if entryPath != targetDir && entryPath != targetPath {
			extras = append(extras, entryPath)
		}
		return nil
	})
	if walkErr != nil {
		return providerInstallError("scan the package directory", targetDir, walkErr)
	}
	if len(extras) > 0 {
		return fmt.Errorf("the package directory contains extra entries; delete the listed entries and run the installer again target_dir=%s\n%s",
			targetDir, strings.Join(extras, "\n"))
	}
	return nil
}

// extractCandidate writes the named archive member into the run staging
// directory with mode 0755 and returns its path and SHA-256 digest. It refuses
// a member that is not a regular file and never uses a header path as a write
// path.
func extractCandidate(archivePath, binaryName, version, runDir string) (candidate, digest string, err error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return "", "", providerInstallError("open the archive", archivePath, err)
	}
	defer archive.Close()
	unzipped, err := gzip.NewReader(archive)
	if err != nil {
		return "", "", providerInstallError("read the gzip stream of", archivePath, err)
	}
	defer unzipped.Close()
	reader := tar.NewReader(unzipped)
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			removeCandidate(candidate)
			return "", "", providerInstallError("read the archive", archivePath, nextErr)
		}
		if candidate != "" || (header.Name != binaryName && header.Name != "./"+binaryName) {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return "", "", fmt.Errorf("archive member %s is not a regular file archive_path=%s", binaryName, archivePath)
		}
		candidate, digest, err = writeCandidate(reader, runDir, binaryName+"_v"+version)
		if err != nil {
			return "", "", err
		}
	}
	if candidate == "" {
		return "", "", fmt.Errorf("archive does not contain the member %s archive_path=%s", binaryName, archivePath)
	}
	// The gzip reader checks the CRC and length trailer only at the end of the
	// stream, so read the stream to its end before the candidate is used.
	drained, drainErr := io.Copy(io.Discard, io.LimitReader(unzipped, maxTrailingBytes))
	if drainErr != nil {
		removeCandidate(candidate)
		return "", "", providerInstallError("read the end of the gzip stream of", archivePath, drainErr)
	}
	if drained == maxTrailingBytes {
		removeCandidate(candidate)
		return "", "", fmt.Errorf("archive has more than %d bytes after its last entry archive_path=%s", int64(maxTrailingBytes), archivePath)
	}
	return candidate, digest, nil
}

func removeCandidate(candidate string) {
	if candidate == "" {
		return
	}
	if err := os.Remove(candidate); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("providers.candidate_cleanup_failed", "candidate", candidate, "err", err)
	}
}

func writeCandidate(member io.Reader, runDir, name string) (candidate, digest string, err error) {
	file, err := os.CreateTemp(runDir, name+".*")
	if err != nil {
		return "", "", providerInstallError("create the candidate in", runDir, err)
	}
	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(member, maxBinaryBytes+1))
	if err != nil {
		_ = file.Close()
		return "", "", providerInstallError("write the candidate", file.Name(), err)
	}
	if written > maxBinaryBytes {
		_ = file.Close()
		return "", "", fmt.Errorf("archive member exceeds %d bytes", int64(maxBinaryBytes))
	}
	if err := file.Chmod(binaryFileMode); err != nil {
		_ = file.Close()
		return "", "", providerInstallError("set the mode of the candidate", file.Name(), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", "", providerInstallError("sync the candidate", file.Name(), err)
	}
	if err := file.Close(); err != nil {
		return "", "", providerInstallError("close the candidate", file.Name(), err)
	}
	slog.Debug("providers.candidate_written", "candidate", file.Name(), "bytes", written)
	return file.Name(), hex.EncodeToString(hasher.Sum(nil)), nil
}

// publishCandidate hard links the candidate to the target. The link fails
// when the target exists, which prevents replacement of an installed binary.
// An identical existing binary stays in place.
func publishCandidate(candidate, targetPath string, row pinRow) (string, error) {
	linkErr := os.Link(candidate, targetPath)
	if linkErr == nil {
		return "installed", nil
	}
	if _, statErr := os.Stat(targetPath); statErr != nil {
		slog.Error("providers.link_failed", "target_path", targetPath, "err", linkErr)
		return "", fmt.Errorf("the link command failed and the target file does not exist target_path=%s link_error=%w", targetPath, linkErr)
	}
	candidateDigest, err := fileSHA256(candidate)
	if err != nil {
		return "", err
	}
	targetDigest, err := fileSHA256(targetPath)
	if err != nil {
		return "", err
	}
	if candidateDigest != targetDigest {
		return "", fmt.Errorf("a different binary exists at the target; if providers.pin assigns a different release archive to an installed mirror version, give the release a new mirror version in providers.pin; if the target file is damaged or partial, delete it and run the installer again source_address=%s version=%s platform=%s tag=%s target_path=%s",
			row.source, row.version, row.platform, row.tag, targetPath)
	}
	return "reused", nil
}

func checkInstalledBinary(targetPath, memberDigest string) error {
	info, err := os.Stat(targetPath)
	if err != nil {
		return providerInstallError("inspect the installed binary", targetPath, err)
	}
	digest, err := fileSHA256(targetPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&executeBits == 0 || digest != memberDigest {
		return fmt.Errorf("binary is not executable or mismatches archive member target_path=%s", targetPath)
	}
	return nil
}
