// Package installedsource verifies nonsecret inputs from an installed host artifact.
package installedsource

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
	"goodkind.io/configsctl/internal/fileowner"
)

// Settings selects one installed artifact and its canonical working directory.
type Settings struct {
	Root        string `yaml:"root"`
	Workspace   string `yaml:"workspace"`
	Node        string `yaml:"node"`
	Environment string `yaml:"environment"`
}

// Validate rejects incomplete selectors and noncanonical root syntax.
func (s Settings) Validate() error {
	if s.Root == "" || s.Workspace == "" || s.Node == "" || s.Environment == "" {
		return errors.New("tofu.installed_source requires root, workspace, node, and environment")
	}
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return errors.New("tofu.installed_source.root must be a clean absolute path")
	}
	return nil
}

// Identity identifies the installed revision that supplied verified inputs.
type Identity struct {
	Root           string
	Workspace      string
	Node           string
	Environment    string
	SourceRevision string
	ManifestSHA256 string
}

// Source records verified API observations without storing secret values.
type Source struct {
	Identity Identity
	files    map[string]transport.HostArtifactFile
	trees    []string
}

// Verify uses the shared API decoder and checks current installed observations.
// Input trees and required files use paths relative to the installed root.
func Verify(settings Settings, currentDirectory string, data []byte, trees, required []string) (*Source, error) {
	slog.Debug("tofu.source.verify", "root", settings.Root, "workspace", settings.Workspace)
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(currentDirectory)
	if err != nil {
		slog.Error("tofu.source.directory_failed", "err", err)
		return nil, fmt.Errorf("resolve the installed working directory: %w", err)
	}
	if canonical != settings.Root {
		return nil, errors.New("the canonical working directory must equal tofu.installed_source.root")
	}
	state, err := transport.DecodeHostArtifactState(data)
	if err != nil {
		slog.Error("tofu.source.decode_failed", "err", err)
		return nil, fmt.Errorf("decode the installed source: %w", err)
	}
	identity, err := verifyIdentity(settings, state)
	if err != nil {
		return nil, err
	}
	source := &Source{
		Identity: identity,
		files:    make(map[string]transport.HostArtifactFile, len(state.Files)), trees: nil,
	}
	if err := source.observe(state); err != nil {
		return nil, err
	}
	for _, tree := range trees {
		path, err := source.Path(tree)
		if err != nil {
			return nil, err
		}
		source.trees = append(source.trees, path)
	}
	for _, path := range source.trees {
		if err := source.walk(path, nil); err != nil {
			return nil, err
		}
	}
	for _, path := range required {
		if _, err := source.Read(path); err != nil {
			return nil, err
		}
	}
	return source, nil
}

func verifyIdentity(settings Settings, state transport.HostArtifactState) (Identity, error) {
	if state.State != transport.HostArtifactStateCommitted || state.Installed == nil {
		return Identity{}, errors.New("the installed source requires a committed installation record")
	}
	installed := state.Installed
	if state.Workspace != settings.Workspace || state.Node != settings.Node || state.Environment != settings.Environment ||
		installed.Workspace != settings.Workspace || installed.Node != settings.Node || installed.Environment != settings.Environment {
		return Identity{}, errors.New("the current and saved installed workspace, node, and environment must match tofu.installed_source")
	}
	if !validHex(installed.SourceRevision, 40) || !validHex(installed.ManifestSHA256, sha256.Size*2) {
		return Identity{}, errors.New("the installed source requires a valid source revision and manifest SHA-256")
	}
	return Identity{
		Root: settings.Root, Workspace: settings.Workspace, Node: settings.Node,
		Environment: settings.Environment, SourceRevision: installed.SourceRevision, ManifestSHA256: installed.ManifestSHA256,
	}, nil
}

func (s *Source) observe(state transport.HostArtifactState) error {
	for _, file := range state.Files {
		if !cleanAbsolute(file.Path) || !file.Present || !file.HasMatches || !file.Matches || !validHex(file.SHA256, sha256.Size*2) {
			return fmt.Errorf("installed file %s must have a matching fresh observation", file.Path)
		}
		if _, duplicate := s.files[file.Path]; duplicate {
			return fmt.Errorf("installed file %s appears more than once", file.Path)
		}
		s.files[file.Path] = file
	}
	for _, directory := range state.Directories {
		if !cleanAbsolute(directory.Path) || !directory.Present || !directory.HasMatches || !directory.Matches {
			return fmt.Errorf("installed directory %s must have a matching fresh observation", directory.Path)
		}
		if err := checkDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func validHex(value string, length int) bool {
	if len(value) != length || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

// InputFiles returns exact installed files inside the runtime root.
// Credential paths and generated plans or state stay at their original locations.
func (s *Source) InputFiles(credentialPaths []string) []string {
	paths := make([]string, 0, len(s.files))
	for path := range s.files {
		relative, err := filepath.Rel(s.Identity.Root, path)
		if err != nil || !filepath.IsLocal(relative) || slices.Contains(credentialPaths, path) || generatedFile(filepath.Base(path)) {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	return paths
}

// AddTree verifies another complete nonsecret input directory.
func (s *Source) AddTree(path string) error {
	absolute, err := s.Path(path)
	if err != nil {
		return err
	}
	if err := s.walk(absolute, nil); err != nil {
		return err
	}
	s.trees = append(s.trees, absolute)
	return nil
}

// Path requires a path inside the canonical installed root without symlinks.
func (s *Source) Path(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.Identity.Root, path)
	}
	relative, err := filepath.Rel(s.Identity.Root, path)
	if err != nil || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("installed input %s must be inside the installed root", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		slog.Error("tofu.source.path_failed", "path", path, "err", err)
		return "", fmt.Errorf("resolve installed input %s: %w", path, err)
	}
	if canonical != path {
		return "", fmt.Errorf("installed input %s must not use symbolic links", path)
	}
	return path, nil
}

// Read checks the file bytes and metadata against the current API observation.
func (s *Source) Read(path string) ([]byte, error) {
	path, err := s.Path(path)
	if err != nil {
		return nil, err
	}
	expected, present := s.files[path]
	if !present {
		return nil, fmt.Errorf("installed input %s is not in the installed file set", path)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("installed input %s must be a regular file", path)
	}
	if err := checkMetadata(path, info, expected.Mode, expected.UID, expected.GID); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		slog.Error("tofu.source.read_failed", "path", path, "err", err)
		return nil, fmt.Errorf("read installed input %s: %w", path, err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		return nil, errors.Join(fmt.Errorf("installed input %s changed while opening it", path), file.Close())
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		slog.Error("tofu.source.read_failed", "path", path, "err", err)
		return nil, fmt.Errorf("read installed input %s: %w", path, err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, after) {
		return nil, fmt.Errorf("installed input %s changed while reading it", path)
	}
	if err := checkMetadata(path, after, expected.Mode, expected.UID, expected.GID); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != expected.Size || hex.EncodeToString(digest[:]) != expected.SHA256 {
		return nil, fmt.Errorf("installed input %s changed after the API observation", path)
	}
	return data, nil
}

func checkMetadata(path string, info fs.FileInfo, mode string, uid, gid int64) error {
	permissions, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || info.Mode().Perm() != fs.FileMode(permissions) {
		return fmt.Errorf("installed input %s does not match its observed mode", path)
	}
	owned, err := fileowner.EffectiveUser(info)
	if err != nil || !owned {
		return fmt.Errorf("installed input %s must belong to the effective user", path)
	}
	return checkOwnerIDs(path, info, uid, gid)
}

func checkDirectory(expected transport.HostArtifactDirectory) error {
	canonical, err := filepath.EvalSymlinks(expected.Path)
	if err != nil || canonical != expected.Path {
		return fmt.Errorf("installed directory %s must not use symbolic links", expected.Path)
	}
	info, err := os.Lstat(expected.Path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("installed directory %s must exist", expected.Path)
	}
	return checkMetadata(expected.Path, info, expected.Mode, expected.UID, expected.GID)
}

func (s *Source) walk(tree string, copyFile func(string, []byte) error) error {
	err := filepath.WalkDir(tree, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Error("tofu.source.enumeration_failed", "path", path, "err", walkErr)
			return fmt.Errorf("enumerate installed input %s: %w", path, walkErr)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("installed input %s must not use symbolic links", path)
		}
		if entry.IsDir() {
			if entry.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return nil
		}
		if generatedFile(entry.Name()) {
			return nil
		}
		data, err := s.Read(path)
		if err != nil {
			return err
		}
		if err := s.checkModules(path, data); err != nil {
			return err
		}
		if copyFile != nil {
			return copyFile(path, data)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify the complete input tree %s: %w", tree, err)
	}
	return nil
}

func (s *Source) checkModules(path string, data []byte) error {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	switch {
	case strings.HasSuffix(path, ".tf"):
		file, diagnostics = parser.ParseHCL(data, path)
	case strings.HasSuffix(path, ".tf.json"):
		file, diagnostics = parser.ParseJSON(data, path)
	default:
		return nil
	}
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.hcl_failed", "path", path, "err", diagnostics.Error())
		return fmt.Errorf("parse installed input %s: %w", path, diagnostics)
	}
	schema := &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "module", LabelNames: []string{"name"}}}}
	content, _, diagnostics := file.Body.PartialContent(schema)
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.module_failed", "path", path, "err", diagnostics.Error())
		return fmt.Errorf("read installed modules %s: %w", path, diagnostics)
	}
	for _, block := range content.Blocks {
		attributes, _, diagnostics := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source", Required: true}}})
		if diagnostics.HasErrors() {
			slog.Error("tofu.source.module_failed", "path", path, "err", diagnostics.Error())
			return fmt.Errorf("read installed module source %s: %w", path, diagnostics)
		}
		value, diagnostics := attributes.Attributes["source"].Expr.Value(nil)
		if diagnostics.HasErrors() || value.IsNull() || !value.Type().Equals(cty.String) {
			return fmt.Errorf("installed module in %s requires a literal local source", path)
		}
		module := value.AsString()
		if !strings.HasPrefix(module, "./") && !strings.HasPrefix(module, "../") {
			return fmt.Errorf("installed module in %s must use a relative local source", path)
		}
		modulePath, err := s.Path(filepath.Join(filepath.Dir(path), module))
		if err != nil {
			return err
		}
		inside := false
		for _, tree := range s.trees {
			relative, err := filepath.Rel(tree, modulePath)
			inside = inside || (err == nil && filepath.IsLocal(relative))
		}
		if !inside {
			return fmt.Errorf("installed module %s must be inside a complete input tree", modulePath)
		}
	}
	return nil
}

func generatedFile(name string) bool {
	return strings.HasSuffix(name, ".tfstate") || strings.HasSuffix(name, ".tfstate.backup") ||
		strings.HasSuffix(name, ".tfplan") || name == ".terraform.tfstate.lock.info"
}

// Copy writes verified nonsecret inputs into a private execution directory.
// Required files outside the complete input trees receive the same byte checks.
func (s *Source) Copy(destination string, required []string) error {
	slog.Debug("tofu.source.copy", "root", s.Identity.Root)
	write := func(path string, data []byte) error {
		relative, err := filepath.Rel(s.Identity.Root, path)
		if err != nil {
			slog.Error("tofu.source.copy_path_failed", "err", err)
			return fmt.Errorf("resolve the copied input path: %w", err)
		}
		output := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			slog.Error("tofu.source.copy_directory_failed", "err", err)
			return fmt.Errorf("create the copied input directory: %w", err)
		}
		permissions, err := strconv.ParseUint(s.files[path].Mode, 8, 32)
		if err != nil {
			slog.Error("tofu.source.copy_mode_failed", "err", err)
			return fmt.Errorf("read the installed input mode: %w", err)
		}
		mode := fs.FileMode(0o600) | fs.FileMode(permissions)&0o100
		if err := os.WriteFile(output, data, mode); err != nil {
			slog.Error("tofu.source.copy_failed", "err", err)
			return fmt.Errorf("copy verified installed input: %w", err)
		}
		copied, err := os.ReadFile(output)
		if err != nil || sha256.Sum256(copied) != sha256.Sum256(data) {
			return errors.New("the copied installed input does not match its API digest")
		}
		return nil
	}
	for _, tree := range s.trees {
		if err := s.walk(tree, write); err != nil {
			return err
		}
	}
	for _, path := range required {
		data, err := s.Read(path)
		if err != nil {
			return err
		}
		absolute, err := s.Path(path)
		if err != nil {
			return err
		}
		if err := write(absolute, data); err != nil {
			return err
		}
	}
	return nil
}
