package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"text/template"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"goodkind.io/configsctl/internal/fileowner"
)

const (
	providerDirectoryMode = 0o700
	providerConfigMode    = 0o600
)

//go:embed providerconfig.tfrc.tmpl
var providerConfigTemplate string

var providerDirectoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type providerTemplateData struct {
	Mirror  cty.Value
	Sources cty.Value
}

func resolveProviderMirror(settingsPath, mirror string) (string, error) {
	if mirror == "" {
		return "", errors.New("tofu.provider_mirror must contain a directory path when declared")
	}
	if !filepath.IsAbs(mirror) {
		mirror = filepath.Join(filepath.Dir(settingsPath), mirror)
	}
	absolute, err := filepath.Abs(mirror)
	if err != nil {
		slog.Error("provider mirror resolution failed", "path", mirror, "err", err)
		return "", fmt.Errorf("resolve tofu.provider_mirror: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		slog.Error("provider mirror inspection failed", "path", absolute, "err", err)
		return "", fmt.Errorf("inspect tofu.provider_mirror %s; run the provider installer: %w", absolute, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("tofu.provider_mirror %s must be a directory", absolute)
	}
	return absolute, nil
}

func tofuProviderConfig(mirror string) (string, error) {
	sources, err := providerMirrorSources(mirror)
	if err != nil {
		slog.Error("provider mirror discovery failed", "path", mirror, "err", err)
		return "", err
	}
	content, err := renderProviderConfig(mirror, sources)
	if err != nil {
		slog.Error("provider configuration rendering failed", "err", err)
		return "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		slog.Error("provider cache resolution failed", "err", err)
		return "", fmt.Errorf("resolve the user cache directory: %w", err)
	}
	if !filepath.IsAbs(cache) {
		return "", errors.New("the user cache directory must be an absolute path")
	}
	path, err := cacheProviderConfig(cache, content)
	if err != nil {
		slog.Error("provider configuration cache failed", "path", cache, "err", err)
		return "", err
	}
	slog.Debug("provider configuration ready", "path", path, "mirror", mirror)
	return path, nil
}

// providerMirrorSources reads directory metadata for source addresses only.
func providerMirrorSources(mirror string) ([]string, error) {
	if !filepath.IsAbs(mirror) {
		return nil, errors.New("tofu.provider_mirror must resolve to an absolute path")
	}
	paths := []string{"."}
	const addressParts = 3
	for range addressParts {
		var children []string
		for _, parent := range paths {
			entries, err := os.ReadDir(filepath.Join(mirror, parent))
			if err != nil {
				slog.Error("provider source directory read failed", "path", filepath.Join(mirror, parent), "err", err)
				return nil, fmt.Errorf("read provider mirror directory %s: %w", filepath.Join(mirror, parent), err)
			}
			for _, entry := range entries {
				if entry.Type()&fs.ModeSymlink != 0 {
					return nil, fmt.Errorf("provider mirror source directory %s must not be a symlink", filepath.Join(parent, entry.Name()))
				}
				if !entry.IsDir() {
					continue
				}
				if !providerDirectoryName.MatchString(entry.Name()) {
					return nil, fmt.Errorf("provider mirror source directory %s contains an invalid address component", filepath.Join(parent, entry.Name()))
				}
				children = append(children, filepath.Join(parent, entry.Name()))
			}
		}
		paths = children
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("provider mirror %s does not contain any source directories; run the provider installer", mirror)
	}
	for index, path := range paths {
		paths[index] = filepath.ToSlash(path)
	}
	return paths, nil
}

func renderProviderConfig(mirror string, sources []string) ([]byte, error) {
	functions := template.FuncMap{
		"hcl": func(value cty.Value) string { return string(hclwrite.TokensForValue(value).Bytes()) },
	}
	parsed, err := template.New("providerconfig").Funcs(functions).Parse(providerConfigTemplate)
	if err != nil {
		slog.Error("provider configuration template parse failed", "err", err)
		return nil, fmt.Errorf("parse provider configuration template: %w", err)
	}
	values := make([]cty.Value, 0, len(sources))
	for _, source := range sources {
		values = append(values, cty.StringVal(source))
	}
	data := providerTemplateData{Mirror: cty.StringVal(mirror), Sources: cty.ListVal(values)}
	var output bytes.Buffer
	if err := parsed.Execute(&output, data); err != nil {
		slog.Error("provider configuration template execution failed", "err", err)
		return nil, fmt.Errorf("render provider configuration template: %w", err)
	}
	return output.Bytes(), nil
}

func cacheProviderConfig(cache string, content []byte) (string, error) {
	if err := os.MkdirAll(cache, providerDirectoryMode); err != nil {
		slog.Error("provider cache directory creation failed", "path", cache, "err", err)
		return "", fmt.Errorf("create the user cache directory: %w", err)
	}
	base, err := os.OpenRoot(cache)
	if err != nil {
		slog.Error("provider cache directory open failed", "path", cache, "err", err)
		return "", fmt.Errorf("open the user cache directory: %w", err)
	}
	defer base.Close()
	application, err := openProviderConfigDirectory(base, "configsctl")
	if err != nil {
		return "", err
	}
	defer application.Close()
	directory, err := openProviderConfigDirectory(application, "providers")
	if err != nil {
		return "", err
	}
	defer directory.Close()
	digest := sha256.Sum256(content)
	name := fmt.Sprintf("%x.tfrc", digest)
	path := filepath.Join(cache, "configsctl", "providers", name)
	if _, err := directory.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		if err := createProviderConfig(directory, name, content); err != nil {
			return "", err
		}
	} else if err != nil {
		slog.Error("provider cached configuration inspection failed", "path", path, "err", err)
		return "", fmt.Errorf("inspect the cached provider configuration: %w", err)
	}
	if err := checkProviderConfig(directory, name, content); err != nil {
		return "", err
	}
	return path, nil
}

func openProviderConfigDirectory(parent *os.Root, name string) (*os.Root, error) {
	if err := parent.Mkdir(name, providerDirectoryMode); err != nil && !errors.Is(err, fs.ErrExist) {
		slog.Error("provider configuration directory creation failed", "name", name, "err", err)
		return nil, fmt.Errorf("create the provider configuration directory: %w", err)
	}
	info, err := parent.Lstat(name)
	if err != nil {
		slog.Error("provider configuration directory inspection failed", "name", name, "err", err)
		return nil, fmt.Errorf("inspect the provider configuration directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != providerDirectoryMode {
		return nil, errors.New("the provider configuration directory must be a real directory with mode 0700")
	}
	if err := checkProviderConfigOwner(info); err != nil {
		return nil, err
	}
	directory, err := parent.OpenRoot(name)
	if err != nil {
		slog.Error("provider configuration directory open failed", "name", name, "err", err)
		return nil, fmt.Errorf("open the provider configuration directory: %w", err)
	}
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = directory.Close()
		return nil, errors.New("the provider configuration directory changed while opening it")
	}
	return directory, nil
}

func checkProviderConfigOwner(info fs.FileInfo) error {
	owned, err := fileowner.CurrentUser(info)
	if err != nil {
		slog.Error("provider configuration ownership inspection failed", "err", err)
		return fmt.Errorf("check the provider configuration owner: %w", err)
	}
	if !owned {
		return errors.New("the current user must own the provider configuration path")
	}
	return nil
}

func createProviderConfig(directory *os.Root, name string, content []byte) error {
	temporary := ".pending-" + rand.Text()
	file, err := directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, providerConfigMode)
	if err != nil {
		slog.Error("provider private configuration creation failed", "name", name, "err", err)
		return fmt.Errorf("create a private provider configuration: %w", err)
	}
	defer directory.Remove(temporary)
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		slog.Error("provider private configuration write failed", "name", name, "err", err)
		return fmt.Errorf("write the private provider configuration: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		slog.Error("provider private configuration sync failed", "name", name, "err", err)
		return fmt.Errorf("sync the private provider configuration: %w", err)
	}
	if err := file.Close(); err != nil {
		slog.Error("provider private configuration close failed", "name", name, "err", err)
		return fmt.Errorf("close the private provider configuration: %w", err)
	}
	if err := directory.Link(temporary, name); err != nil && !errors.Is(err, fs.ErrExist) {
		slog.Error("provider private configuration save failed", "name", name, "err", err)
		return fmt.Errorf("save the private provider configuration: %w", err)
	}
	return nil
}

func checkProviderConfig(directory *os.Root, name string, content []byte) error {
	info, err := directory.Lstat(name)
	if err != nil {
		slog.Error("provider private configuration inspection failed", "name", name, "err", err)
		return fmt.Errorf("inspect the private provider configuration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != providerConfigMode {
		return errors.New("the provider configuration must be a regular file with mode 0600 and must not be a symlink")
	}
	if err := checkProviderConfigOwner(info); err != nil {
		return err
	}
	file, err := directory.Open(name)
	if err != nil {
		slog.Error("provider private configuration open failed", "name", name, "err", err)
		return fmt.Errorf("open the private provider configuration: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("the provider configuration changed while opening it")
	}
	if info.Size() != int64(len(content)) {
		return errors.New("the cached provider configuration does not match its content digest")
	}
	data := make([]byte, len(content))
	if _, err := file.ReadAt(data, 0); err != nil {
		slog.Error("provider cached configuration read failed", "name", name, "err", err)
		return fmt.Errorf("read the cached provider configuration: %w", err)
	}
	if !bytes.Equal(data, content) {
		return errors.New("the cached provider configuration does not match its content digest")
	}
	return nil
}
