package guestrefs

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/zclconf/go-cty/cty"
)

type locations map[string]hcl.Range

func sourceLocations(repository, renderRoot, source string) (locations, error) {
	path := source
	if !filepath.IsAbs(path) {
		path = filepath.Join(renderRoot, path)
	}

	relative, err := filepath.Rel(repository, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, validationError(
			"guest_references.source must identify a file inside the repository",
		)
	}

	directory, err := os.OpenRoot(repository)
	if err != nil {
		slog.Error("guestrefs.repository_failed", "err", err)

		return nil, fmt.Errorf("open native configuration repository: %w", err)
	}
	defer directory.Close()
	body, err := directory.ReadFile(relative)
	if err != nil {
		slog.Error("guestrefs.read_failed", "path", relative, "err", err)

		return nil, fmt.Errorf("read guest reference source: %w", err)
	}

	parser := hclparse.NewParser()
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(relative, ".json") {
		file, diagnostics = parser.ParseJSON(body, relative)
	} else {
		file, diagnostics = parser.ParseHCL(body, relative)
	}

	if diagnostics.HasErrors() {
		return nil, validationError("invalid guest reference source " + relative)
	}

	positions := locations{}
	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "locals"},
			{Type: "output", LabelNames: []string{"name"}},
		},
	}

	blocks, _, _ := file.Body.PartialContent(schema)
	for _, block := range blocks.Blocks {
		attrs, _ := block.Body.JustAttributes()
		if block.Type == "output" {
			if value := attrs["value"]; value != nil {
				positions.record(block.Labels[0], value.Expr)
			}
		} else {
			for name, attr := range attrs {
				positions.record(name, attr.Expr)
			}
		}
	}

	return positions, nil
}

func (lines locations) record(path string, expr hcl.Expression) {
	if _, found := lines[path]; !found {
		lines[path] = expr.Range()
	}

	if call, err := hcl.ExprCall(expr); !err.HasErrors() && call.Name == "merge" {
		for _, arg := range call.Arguments {
			lines.record(path, arg)
		}

		return
	}

	entries, err := hcl.ExprMap(expr)
	if err.HasErrors() {
		return
	}

	for _, entry := range entries {
		key, err := entry.Key.Value(nil)
		if err.HasErrors() || !key.IsKnown() || key.IsNull() || key.Type() != cty.String {
			continue
		}

		child := path + "\x00" + key.AsString()
		lines[child] = entry.Key.Range()
		lines.record(child, entry.Value)
	}
}

func (config workspace) location(section string, keys ...string) string {
	path := append([]string{section}, keys...)
	for length := len(path); length > 0; length-- {
		if position, found := config.lines[strings.Join(path[:length], "\x00")]; found {
			return fmt.Sprintf("%s:%d", position.Filename, position.Start.Line)
		}
	}

	// A custom projection can construct guest_references directly in an output.
	aliases := map[string]string{
		"managed_guests":     "declared",
		"service_mapping":    "services",
		"guest_locks":        "locks",
		"keyed_guest_locks":  "keyed_locks",
		"static_guest_locks": "static_locks",
	}
	if alias, found := aliases[section]; found {
		path = append([]string{"guest_references", alias}, keys...)
		for length := len(path); length > 1; length-- {
			if position, found := config.lines[strings.Join(path[:length], "\x00")]; found {
				return fmt.Sprintf("%s:%d", position.Filename, position.Start.Line)
			}
		}
	}

	if position, found := config.lines["guest_references"]; found {
		return fmt.Sprintf("%s:%d", position.Filename, position.Start.Line)
	}

	return config.Source
}
