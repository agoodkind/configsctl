package tofurun

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"goodkind.io/configsctl/internal/fileowner"
)

const (
	privateFileMode      fs.FileMode = 0o600
	privateDirectoryMode fs.FileMode = 0o700
	// groupOrOtherAccess covers every permission bit for group and other users.
	groupOrOtherAccess fs.FileMode = 0o077
	providerLockFile               = ".terraform.lock.hcl"
	workspaceFile                  = ".terraform/environment"
	defaultWorkspace               = "default"
	savedPlanPattern               = "tofurun-plan-*"
)

func checkLocations(request Request) error {
	if !cleanAbsolute(request.Binary) {
		return rejection("tofurun.binary_refused", "Binary must be a clean absolute path")
	}
	binary, err := os.Stat(request.Binary)
	if err != nil || !binary.Mode().IsRegular() || binary.Mode().Perm()&0o111 == 0 {
		return rejection("tofurun.binary_refused", request.Binary+" is not an executable file")
	}
	if !cleanAbsolute(request.Root) {
		return rejection("tofurun.root_refused", "Root must be a clean absolute path")
	}
	root, err := os.Stat(request.Root)
	if err != nil || !root.IsDir() {
		return rejection("tofurun.root_refused", request.Root+" is not a directory")
	}
	if request.Workspace == "" {
		return rejection("tofurun.workspace_refused", "Workspace must name an existing OpenTofu workspace")
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

// checkPrivateDirectory requires an existing directory that the current user
// owns, that is not a symbolic link, and that denies group and other access.
func checkPrivateDirectory(label, path string) error {
	if !cleanAbsolute(path) {
		return rejection("tofurun.directory_refused", label+" must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return rejection("tofurun.directory_refused", fmt.Sprintf("%s: %v", label, err))
	}
	return checkPrivateDirectoryInfo(label, path, info)
}

// checkPrivateDirectoryInfo applies the checkPrivateDirectory rules to info
// from Lstat.
func checkPrivateDirectoryInfo(label, path string, info fs.FileInfo) error {
	switch {
	case !info.IsDir():
		return rejection("tofurun.directory_refused", fmt.Sprintf("%s %s is not a directory", label, path))
	case info.Mode().Perm()&groupOrOtherAccess != 0:
		return rejection("tofurun.directory_refused", fmt.Sprintf("%s %s has mode %v; group and other users must not have access", label, path, info.Mode().Perm()))
	}
	return checkOwner(label, path, info)
}

// checkPrivateFile requires a regular file in a private directory. The
// current user must own the file, and group and other users must not have access.
func checkPrivateFile(label, path string) error {
	if !cleanAbsolute(path) {
		return rejection("tofurun.file_refused", fmt.Sprintf("the file of %s must be a clean absolute path", label))
	}
	if err := checkPrivateDirectory(label+" directory", filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return rejection("tofurun.file_refused", fmt.Sprintf("the file of %s: %v", label, err))
	case !info.Mode().IsRegular():
		return rejection("tofurun.file_refused", fmt.Sprintf("the file of %s is not a regular file", label))
	case info.Mode().Perm()&groupOrOtherAccess != 0:
		return rejection("tofurun.file_refused", fmt.Sprintf("the file of %s has mode %v; group and other users must not have access", label, info.Mode().Perm()))
	}
	return checkOwner(label, path, info)
}

func checkOwner(label, path string, info fs.FileInfo) error {
	owned, err := fileowner.CurrentUser(info)
	if err != nil {
		return rejection("tofurun.owner_unknown", fmt.Sprintf("%s %s: %v", label, path, err))
	}
	if !owned {
		return rejection("tofurun.owner_refused", fmt.Sprintf("%s %s belongs to another user", label, path))
	}
	return nil
}

// within reports whether path is directory or a path below it.
func within(directory, path string) bool {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

// checkSelectedWorkspace requires the workspace that "workspace select"
// selected in root. OpenTofu writes the selected name to workspaceFile and
// uses the default workspace without that file.
func checkSelectedWorkspace(root, workspace string) error {
	body, err := os.ReadFile(filepath.Join(root, workspaceFile))
	selected := strings.TrimSpace(string(body))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		selected = defaultWorkspace
	case err != nil:
		return rejection("tofurun.workspace_unreadable", fmt.Sprintf("read the selected workspace: %v", err))
	}
	if selected != workspace {
		return rejection("tofurun.workspace_refused", fmt.Sprintf("the selected workspace is %q, not %q; run workspace select first", selected, workspace))
	}
	return nil
}

// checkOutput checks JSONPath for PrivateJSON. JSONPath must be a new file in
// a private directory below TMPDIR.
func checkOutput(request Request, parsed command, built environment) error {
	if request.OutputMode != PrivateJSON {
		if request.JSONPath != "" {
			return rejection("tofurun.output_refused", "JSONPath requires private_json output")
		}
		return nil
	}
	if parsed.name != showCommand {
		return rejection("tofurun.output_refused", "private_json output requires show -json <saved-plan>")
	}
	_, err := privateOutputPath("JSONPath", built.tmpdir, request.JSONPath)
	return err
}

// privateOutputPath returns path relative to tmpdir for a new output file.
// It opens an [os.Root] at tmpdir and checks each directory between tmpdir and
// the file. Each directory must be private and must not be a symbolic link.
// The file must not exist.
func privateOutputPath(label, tmpdir, path string) (string, error) {
	if !cleanAbsolute(path) || !within(tmpdir, path) || path == tmpdir {
		return "", rejection("tofurun.output_path_refused", label+" must be a clean absolute path below TMPDIR")
	}
	relative, err := filepath.Rel(tmpdir, path)
	if err != nil {
		return "", rejection("tofurun.output_path_refused", fmt.Sprintf("%s: %v", label, err))
	}
	root, err := os.OpenRoot(tmpdir)
	if err != nil {
		return "", rejection("tofurun.output_path_refused", fmt.Sprintf("open TMPDIR for %s: %v", label, err))
	}
	defer closeRoot(root)
	directory := ""
	for part := range strings.SplitSeq(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." {
			break
		}
		directory = filepath.Join(directory, part)
		info, err := root.Lstat(directory)
		if err != nil {
			return "", rejection("tofurun.output_path_refused", fmt.Sprintf("%s directory %s: %v", label, directory, err))
		}
		if err := checkPrivateDirectoryInfo(label+" directory", filepath.Join(tmpdir, directory), info); err != nil {
			return "", err
		}
	}
	if _, err := root.Lstat(relative); !errors.Is(err, fs.ErrNotExist) {
		return "", rejection("tofurun.output_path_refused", fmt.Sprintf("cannot use %s %s: the output file must not exist", label, path))
	}
	return relative, nil
}

func closeRoot(root *os.Root) {
	if err := root.Close(); err != nil {
		slog.Warn("tofurun.close_failed", "path", root.Name(), "err", err)
	}
}

// copySavedPlan copies the saved plan of show or apply to a new private
// directory below TMPDIR and replaces the argument with the path of the copy.
// OpenTofu reads only the copy.
func (p *prepared) copySavedPlan() error {
	index := p.command.savedPlanIndex()
	if index < 0 {
		return nil
	}
	source := p.args[index]
	if !filepath.IsAbs(source) {
		source = filepath.Join(p.request.Root, source)
	}
	directory, err := os.MkdirTemp(p.environment.tmpdir, savedPlanPattern)
	if err != nil {
		slog.Error("tofurun.saved_plan_directory_failed", "err", err)
		return fmt.Errorf("create the private saved plan directory: %w", err)
	}
	p.removals = append(p.removals, directory)
	if err := os.Chmod(directory, privateDirectoryMode); err != nil {
		slog.Error("tofurun.saved_plan_directory_failed", "dir", directory, "err", err)
		return fmt.Errorf("restrict the private saved plan directory: %w", err)
	}
	destination := filepath.Join(directory, filepath.Base(source))
	if err := copyRegularFile(source, destination); err != nil {
		return err
	}
	p.args[index] = destination
	return nil
}

// copyRegularFile copies a nonempty regular file that is not a symbolic link
// to a new private file.
func copyRegularFile(source, destination string) error {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() || before.Size() == 0 {
		return rejection("tofurun.saved_plan_refused", fmt.Sprintf("the saved plan %s must be a nonempty regular file", source))
	}
	input, err := os.Open(source)
	if err != nil {
		return rejection("tofurun.saved_plan_refused", fmt.Sprintf("open the saved plan: %v", err))
	}
	defer closeQuietly(input)
	opened, err := input.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return rejection("tofurun.saved_plan_refused", fmt.Sprintf("the saved plan %s changed while the runner opened it", source))
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, privateFileMode)
	if err != nil {
		slog.Error("tofurun.saved_plan_copy_failed", "path", destination, "err", err)
		return fmt.Errorf("create the private saved plan: %w", err)
	}
	copied, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		slog.Error("tofurun.saved_plan_copy_failed", "path", destination, "err", err)
		return fmt.Errorf("copy the saved plan: %w", err)
	}
	if copied == 0 {
		return rejection("tofurun.saved_plan_refused", fmt.Sprintf("the saved plan %s is empty", source))
	}
	return nil
}

func closeQuietly(file *os.File) {
	if err := file.Close(); err != nil {
		slog.Warn("tofurun.close_failed", "path", file.Name(), "err", err)
	}
}

// providerLockDigest returns the SHA-256 digest of the provider lock file
// for init, or nil for another command or a missing lock file.
func (p *prepared) providerLockDigest() ([]byte, error) {
	if p.command.name != initCommand {
		return nil, nil
	}
	body, err := os.ReadFile(filepath.Join(p.request.Root, providerLockFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		slog.Error("tofurun.provider_lock_unreadable", "root", p.request.Root, "err", err)
		return nil, fmt.Errorf("read the provider lock file: %w", err)
	}
	digest := sha256.Sum256(body)
	return digest[:], nil
}

// checkProviderLock compares the provider lock digest after init with the
// digest before init.
func (p *prepared) checkProviderLock(before []byte) error {
	after, err := p.providerLockDigest()
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		slog.Error("tofurun.provider_lock_changed", "root", p.request.Root, "err", ErrProviderLockChanged)
		return fmt.Errorf("%w in %s", ErrProviderLockChanged, p.request.Root)
	}
	return nil
}
