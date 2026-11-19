package retirement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"goodkind.io/configsctl/internal/tofufiles"
)

// graph lists the removed addresses for each backend directory.
type graph struct {
	roots  map[string]map[string]bool
	unused map[string]map[string]bool
}

func (s *snapshot) graph(ctx context.Context, workspaces string) (graph, error) {
	result := graph{roots: map[string]map[string]bool{}, unused: map[string]map[string]bool{}}
	memo := map[string]map[string]bool{}
	candidates := []string{}

	for dir, entries := range s.files {
		if dir != workspaces && workspaces != "." && !strings.HasPrefix(dir, workspaces+"/") {
			continue
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(dir, workspaces), "/")
		parts := strings.Split(rel, "/")
		if slices.ContainsFunc(
			parts,
			func(part string) bool {
				return (part != "." && strings.HasPrefix(part, ".")) || part == "terraform.d"
			},
		) {
			continue
		}

		if !slices.ContainsFunc(
			slices.Collect(maps.Keys(entries)),
			func(name string) bool { return tofufiles.Active(name, nil) },
		) {
			continue
		}

		candidates = append(candidates, dir)
	}

	slices.Sort(candidates)

	for _, dir := range candidates {
		configuration, err := s.load(ctx, dir)
		if err != nil {
			return result, err
		}

		if configuration.backend == nil {
			continue
		}

		targets, err := s.targets(ctx, dir, memo, map[string]bool{})
		if err != nil {
			slog.Error("retirement.backend_graph_failed", "directory", dir, "err", err)

			return result, fmt.Errorf("resolve consumers of backend %q: %w", dir, err)
		}

		result.roots[dir] = targets
	}

	for _, dir := range candidates {
		if _, used := memo[dir]; !used {
			result.unused[dir] = s.modules[dir].removed
		}
	}

	return result, nil
}

func (s *snapshot) targets(
	ctx context.Context,
	dir string,
	memo map[string]map[string]bool,
	visiting map[string]bool,
) (map[string]bool, error) {
	if memo[dir] != nil {
		return memo[dir], nil
	}

	if visiting[dir] {
		return nil, errModuleCycle
	}

	visiting[dir] = true
	defer delete(visiting, dir)
	configuration, err := s.load(ctx, dir)
	if err != nil {
		return nil, err
	}

	targets := maps.Clone(configuration.removed)
	for name, source := range configuration.calls {
		if !hclsyntax.ValidIdentifier(name) ||
			(!strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../")) {
			return nil, errModuleSource
		}

		target := path.Join(dir, source)
		if target == ".." || strings.HasPrefix(target, "../") {
			return nil, errEscapingSource
		}

		children, err := s.targets(ctx, target, memo, visiting)
		if err != nil {
			slog.Error("retirement.module_graph_failed", "directory", dir, "err", err)

			return nil, fmt.Errorf("resolve local module in %q: %w", dir, err)
		}

		for child := range children {
			targets["module."+name+"."+child] = true
		}
	}

	memo[dir] = targets

	return targets, nil
}

// validTarget checks the module path, resource name, and instance keys.
func validTarget(steps hcl.Traversal) error {
	invalid := errTargetType

	for len(steps) > 0 {
		kind, ok := stepName(steps[0])
		if !ok {
			return invalid
		}

		steps = steps[1:]
		if kind == "data" {
			if len(steps) == 0 {
				return invalid
			}

			if _, ok := stepName(steps[0]); !ok {
				return invalid
			}

			steps = steps[1:]
		}

		if len(steps) == 0 {
			return invalid
		}

		if _, ok := stepName(steps[0]); !ok {
			return invalid
		}

		steps = steps[1:]
		if len(steps) > 0 {
			if index, ok := steps[0].(hcl.TraverseIndex); ok {
				if index.Key.Type() != cty.String && index.Key.Type() != cty.Number {
					return invalid
				}

				steps = steps[1:]
			}
		}

		if len(steps) > 0 && blockKind(kind) != moduleBlock {
			return invalid
		}
	}

	return nil
}

func stepName(step hcl.Traverser) (string, bool) {
	switch s := step.(type) {
	case hcl.TraverseRoot:
		return s.Name, true
	case hcl.TraverseAttr:
		return s.Name, true
	default:
		return "", false
	}
}

func matches(target, record hcl.Traversal) bool {
	for _, wanted := range target {
		if len(record) == 0 {
			return false
		}

		if _, indexed := record[0].(hcl.TraverseIndex); indexed {
			if _, specified := wanted.(hcl.TraverseIndex); !specified {
				record = record[1:]
			}
		}

		if len(record) == 0 {
			return false
		}

		if index, ok := wanted.(hcl.TraverseIndex); ok {
			actual, ok := record[0].(hcl.TraverseIndex)
			if !ok || !index.Key.RawEquals(actual.Key) {
				return false
			}
		} else {
			want, _ := stepName(wanted)
			actual, ok := stepName(record[0])
			if !ok || want != actual {
				return false
			}
		}

		record = record[1:]
	}

	return true
}

var (
	errModuleCycle    = errors.New("the local module graph contains a cycle")
	errModuleSource   = errors.New("a module call requires a static local source")
	errEscapingSource = errors.New("a module source is outside the reviewed tree")
	errTargetType     = errors.New("removed.from must select a module or a resource")
)
