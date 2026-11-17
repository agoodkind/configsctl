// Package retirement verifies the state of retired OpenTofu addresses.
package retirement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"go.yaml.in/yaml/v3"
	"goodkind.io/configsctl/internal/tofufiles"
)

type blockKind string

const (
	moduleBlock    blockKind = "module"
	removedBlock   blockKind = "removed"
	variableBlock  blockKind = "variable"
	terraformBlock blockKind = "terraform"
)

var commitID = regexp.MustCompile("^(?:[0-9a-f]{40}|[0-9a-f]{64})$")

type (
	treeFile struct{ mode, id string }
	snapshot struct {
		checkout string
		files    map[string]map[string]treeFile
		modules  map[string]*module
		blobs    map[string][]byte
	}
)

type sourceBlock struct {
	block  *hcl.Block
	data   []byte
	native *hclsyntax.Block
}
type module struct {
	backend       *sourceBlock
	encryption    *sourceBlock
	stateVariable *sourceBlock
	calls         map[string]string
	removed       map[string]bool
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "--no-replace-objects")
	cmd.Dir = dir
	cmd.Args = append(cmd.Args, args...)
	out, err := cmd.Output()
	if err != nil {
		slog.ErrorContext(ctx, "retirement.git_read_failed", "err", errGitRead)

		return nil, errGitRead
	}

	return out, nil
}

func readSnapshot(
	ctx context.Context,
	checkout string,
	rev string,
	blobs map[string][]byte,
) (*snapshot, error) {
	if !commitID.MatchString(rev) {
		return nil, errCommitID
	}

	kind, err := git(ctx, checkout, "cat-file", "-t", rev)
	if err != nil || string(kind) != "commit\n" {
		return nil, errCommitType
	}

	data, err := git(ctx, checkout, "ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}

	tree := &snapshot{
		checkout: checkout,
		files:    map[string]map[string]treeFile{},
		modules:  map[string]*module{},
		blobs:    blobs,
	}

	for entry := range bytes.SplitSeq(data, []byte{0}) {
		if len(entry) == 0 {
			continue
		}

		meta, name, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, errTreeEntry
		}

		dir, base := path.Dir(name), path.Base(name)
		if tree.files[dir] == nil {
			tree.files[dir] = map[string]treeFile{}
		}

		tree.files[dir][base] = treeFile{fields[0], fields[2]}
	}

	return tree, nil
}

func (s *snapshot) read(ctx context.Context, file treeFile) ([]byte, error) {
	if file.mode != "100644" && file.mode != "100755" {
		return nil, errSourceMode
	}

	if data, ok := s.blobs[file.id]; ok {
		return data, nil
	}

	data, err := git(ctx, s.checkout, "cat-file", "blob", file.id)
	if err == nil {
		s.blobs[file.id] = data
	}

	return data, err
}

func (s *snapshot) workspaceDir(ctx context.Context) (string, error) {
	data, err := s.read(ctx, s.files["."]["configsctl.yml"])
	if err != nil {
		return "", err
	}

	var settings struct {
		Tofu struct {
			WorkspacesDir string `yaml:"workspaces_dir"`
		} `yaml:"tofu"`
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return "", errSettings
	}

	dir := settings.Tofu.WorkspacesDir
	if !filepath.IsLocal(dir) {
		return "", errWorkspaceDirectory
	}

	return path.Clean(dir), nil
}

var (
	topSchema = hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: string(terraformBlock)},
			{Type: string(moduleBlock), LabelNames: []string{"name"}},
			{Type: string(removedBlock)},
			{Type: string(variableBlock), LabelNames: []string{"name"}},
		},
	}
	stateSchema = hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "backend", LabelNames: []string{"type"}},
			{Type: "encryption"},
		},
	}
)

func (s *snapshot) load(ctx context.Context, dir string) (*module, error) {
	if configuration := s.modules[dir]; configuration != nil {
		return configuration, nil
	}

	configuration := &module{removed: map[string]bool{}, calls: map[string]string{}}
	entries := s.files[dir]
	names := map[string]bool{}

	for name := range entries {
		names[name] = true
	}

	active := false

	for name, file := range entries {
		if !tofufiles.Active(name, names) {
			continue
		}

		active = true
		data, err := s.read(ctx, file)
		if err != nil {
			return nil, err
		}

		if err := configuration.parse(path.Join(dir, name), data); err != nil {
			slog.Error("retirement.configuration_invalid", "directory", dir, "err", err)

			return nil, fmt.Errorf("inspect active OpenTofu configuration in %q: %w", dir, err)
		}
	}

	if !active {
		return nil, errEmptyModule
	}

	s.modules[dir] = configuration

	return configuration, nil
}

func (configuration *module) parse(name string, data []byte) error {
	parser := hclparse.NewParser()
	var file *hcl.File
	var diag hcl.Diagnostics
	if strings.HasSuffix(name, ".json") {
		file, diag = parser.ParseJSON(data, name)
	} else {
		file, diag = parser.ParseHCL(data, name)
	}

	if diag.HasErrors() {
		return errSyntax
	}

	top, _, diag := file.Body.PartialContent(&topSchema)
	if diag.HasErrors() {
		return errBlockSchema
	}

	for _, block := range top.Blocks {
		if err := configuration.parseBlock(file.Body, block, data); err != nil {
			return err
		}
	}

	return nil
}

func (configuration *module) parseBlock(parent hcl.Body, block *hcl.Block, data []byte) error {
	switch blockKind(block.Type) {
	case moduleBlock:
		expr, err := required(block, "source")
		if err != nil {
			return err
		}

		value, diag := expr.Expr.Value(nil)
		// Resolve local calls when walking the graph. An unresolved call cannot prove absence.
		source := "expression:" + string(expr.Expr.Range().SliceBytes(data))
		if !diag.HasErrors() && !value.IsNull() && value.IsKnown() &&
			value.Type() == cty.String {
			source = value.AsString()
		}

		if _, ok := configuration.calls[block.Labels[0]]; ok {
			return errDuplicateModule
		}

		configuration.calls[block.Labels[0]] = source
	case removedBlock:
		expr, err := required(block, "from")
		if err != nil {
			return err
		}

		traversal, diag := hcl.AbsTraversalForExpr(expr.Expr)
		if diag.HasErrors() {
			return errStaticAddress
		}

		if err := validTarget(traversal); err != nil {
			return err
		}

		configuration.removed[string(hclwrite.TokensForTraversal(traversal).Bytes())] = true
	case terraformBlock:
		children, _, diag := block.Body.PartialContent(&stateSchema)
		if diag.HasErrors() {
			return errBackendSchema
		}

		for _, child := range children.Blocks {
			dest := &configuration.backend
			if child.Type == "encryption" {
				dest = &configuration.encryption
			}

			if *dest != nil {
				return errDuplicateBackend
			}

			*dest = selectBlock(block.Body, child, data)
		}
	case variableBlock:
		if block.Labels[0] == "vault_tofu_state_passphrase" {
			configuration.stateVariable = selectBlock(parent, block, data)
		}
	}

	return nil
}

func selectBlock(parent hcl.Body, block *hcl.Block, data []byte) *sourceBlock {
	selected := &sourceBlock{block: block, data: data}
	if body, ok := parent.(*hclsyntax.Body); ok {
		for _, item := range body.Blocks {
			if item.TypeRange == block.TypeRange {
				selected.native = item

				break
			}
		}
	}

	return selected
}

func required(block *hcl.Block, name string) (*hcl.Attribute, error) {
	body, _, diag := block.Body.PartialContent(&hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{{Name: name, Required: true}},
	})
	if diag.HasErrors() {
		slog.Error("retirement.attribute_missing", "attribute", name, "err", errRequiredAttribute)

		return nil, fmt.Errorf("%w: %s.%s", errRequiredAttribute, block.Type, name)
	}

	return body.Attributes[name], nil
}

var (
	errGitRead            = errors.New("could not read the specified Git commit or source object")
	errCommitID           = errors.New("supply the full immutable Git commit ID")
	errCommitType         = errors.New("could not verify the supplied Git commit ID")
	errTreeEntry          = errors.New("invalid Git tree entry")
	errSourceMode         = errors.New("the source must be a regular tracked file")
	errSettings           = errors.New("invalid configsctl.yml")
	errWorkspaceDirectory = errors.New("configure tofu.workspaces_dir inside the repository")
	errEmptyModule        = errors.New(
		"a local module does not contain tracked OpenTofu configuration",
	)
	errSyntax           = errors.New("invalid OpenTofu syntax")
	errBlockSchema      = errors.New("invalid OpenTofu blocks")
	errDuplicateModule  = errors.New("duplicate or overridden module call")
	errStaticAddress    = errors.New("removed.from requires a static address")
	errBackendSchema    = errors.New("invalid backend or encryption configuration")
	errDuplicateBackend = errors.New(
		"duplicate or overridden backend or encryption configuration",
	)
	errRequiredAttribute = errors.New("missing required attribute")
)
