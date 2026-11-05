package installedsource

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

// RequirePersistentBackend refuses local state paths inside the disposable copy.
// Remote backends preserve their existing configuration.
func (s *Source) RequirePersistentBackend(workspace, selected, copyRoot string) error {
	backends, err := s.localBackends(workspace)
	if err != nil {
		return err
	}
	for _, body := range backends {
		if err := requireLocalBackendPath(body, "path", copyRoot); err != nil {
			return err
		}
		if selected != "default" {
			if err := requireLocalBackendPath(body, "workspace_dir", copyRoot); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Source) localBackends(workspace string) ([]hcl.Body, error) {
	directory, err := s.Path(workspace)
	if err != nil {
		return nil, err
	}
	var backends []hcl.Body
	for path := range s.files {
		if filepath.Dir(path) != directory || (!strings.HasSuffix(path, ".tf") && !strings.HasSuffix(path, ".tf.json")) {
			continue
		}
		data, err := s.Read(path)
		if err != nil {
			return nil, err
		}
		file, err := parseInputFile(path, data)
		if err != nil {
			return nil, err
		}
		bodies, err := localBackendBodies(file.Body)
		if err != nil {
			return nil, err
		}
		backends = append(backends, bodies...)
	}
	return backends, nil
}

func parseInputFile(path string, data []byte) (*hcl.File, error) {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(path, ".json") {
		file, diagnostics = parser.ParseJSON(data, path)
	} else {
		file, diagnostics = parser.ParseHCL(data, path)
	}
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.backend_parse_failed", "path", path, "err", diagnostics.Error())
		return nil, fmt.Errorf("parse the installed backend configuration: %w", diagnostics)
	}
	return file, nil
}

func localBackendBodies(body hcl.Body) ([]hcl.Body, error) {
	terraform, _, diagnostics := body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}})
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.backend_read_failed", "err", diagnostics.Error())
		return nil, fmt.Errorf("read the installed backend configuration: %w", diagnostics)
	}
	var bodies []hcl.Body
	for _, block := range terraform.Blocks {
		backends, _, diagnostics := block.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}}})
		if diagnostics.HasErrors() {
			slog.Error("tofu.source.backend_read_failed", "err", diagnostics.Error())
			return nil, fmt.Errorf("read the installed backend: %w", diagnostics)
		}
		for _, backend := range backends.Blocks {
			if backend.Labels[0] != "local" {
				continue
			}
			bodies = append(bodies, backend.Body)
		}
	}
	return bodies, nil
}

// RequireBackendOverride checks actual local backend options after CLI expansion.
func (s *Source) RequireBackendOverride(workspace, option, copyRoot string) error {
	backends, err := s.localBackends(workspace)
	if err != nil {
		return err
	}
	if len(backends) == 0 {
		return nil
	}
	name, value, assigned := strings.Cut(option, "=")
	if assigned {
		if name == "path" || name == "workspace_dir" {
			return requireAbsoluteBackendPath(name, value, copyRoot)
		}
		return nil
	}
	path := option
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	data, err := s.Read(path)
	if err != nil {
		return err
	}
	file, err := parseInputFile(path, data)
	if err != nil {
		return err
	}
	content, _, diagnostics := file.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{
		{Name: "path", Required: false}, {Name: "workspace_dir", Required: false},
	}})
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.backend_override_failed", "err", diagnostics.Error())
		return fmt.Errorf("read the installed backend option file: %w", diagnostics)
	}
	for name := range content.Attributes {
		if err := requireLocalBackendPath(file.Body, name, copyRoot); err != nil {
			return err
		}
	}
	return nil
}

func requireLocalBackendPath(body hcl.Body, name, copyRoot string) error {
	content, _, diagnostics := body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: name, Required: false}}})
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.backend_path_failed", "err", diagnostics.Error())
		return fmt.Errorf("read the installed local backend path: %w", diagnostics)
	}
	action := "set the local backend " + name + " to an absolute path outside the private source copy before running installed source"
	attribute, present := content.Attributes[name]
	if !present {
		return errors.New(action)
	}
	value, diagnostics := attribute.Expr.Value(nil)
	if diagnostics.HasErrors() || value.IsNull() || !value.Type().Equals(cty.String) {
		return errors.New(action)
	}
	return requireAbsoluteBackendPath(name, value.AsString(), copyRoot)
}

func requireAbsoluteBackendPath(name, path, copyRoot string) error {
	relative, err := filepath.Rel(copyRoot, path)
	if !cleanAbsolute(path) || (err == nil && filepath.IsLocal(relative)) {
		return errors.New("set the local backend " + name + " to an absolute path outside the private source copy before running installed source")
	}
	return nil
}
