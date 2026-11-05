package installedsource

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
)

// FileInput reports whether an option reads a nonsecret source file.
func FileInput(name string) bool {
	return name == "var-file" || name == "backend-config" || name == "target-file" || name == "exclude-file"
}

// CopiedInput records the original input and its verified private copy.
type CopiedInput struct {
	Original string
	Path     string
}

// CopyFileArgument verifies a supported source-file option and maps its path.
func (s *Source) CopyFileArgument(destination, workspace, name, value string, credentialPaths []string) (CopiedInput, error) {
	if !FileInput(name) {
		return CopiedInput{}, fmt.Errorf("OpenTofu option %s does not select a source file", name)
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	return s.CopyInputFile(destination, path, credentialPaths)
}

// CopyInputFile checks original bytes and excludes files used for credentials.
func (s *Source) CopyInputFile(destination, path string, credentialPaths []string) (CopiedInput, error) {
	absolute, err := s.Path(path)
	if err != nil {
		return CopiedInput{}, err
	}
	if slices.Contains(credentialPaths, absolute) {
		return CopiedInput{}, errors.New("credential files must retain their original paths and cannot be OpenTofu source arguments")
	}
	if err := s.Copy(destination, []string{absolute}); err != nil {
		return CopiedInput{}, err
	}
	relative, err := filepath.Rel(s.Identity.Root, absolute)
	if err != nil {
		slog.Error("tofu.source.argument_path_failed", "err", err)
		return CopiedInput{}, fmt.Errorf("resolve the verified source argument: %w", err)
	}
	return CopiedInput{Original: absolute, Path: filepath.Join(destination, relative)}, nil
}
