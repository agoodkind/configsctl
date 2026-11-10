package installedsource

import (
	"fmt"
	"log/slog"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"goodkind.io/configsctl/internal/tofufiles"
)

type moduleDependency struct {
	directory string
	inputs    map[string]cty.Value
}

func (s *Source) moduleContext(
	directory string,
	inputs map[string]cty.Value,
) (*hcl.EvalContext, error) {
	native, jsonFiles, err := tofufiles.Discover(directory)
	if err != nil {
		slog.Error("tofu.source.variable_discovery_failed", "err", err)
		return nil, fmt.Errorf("discover installed module variables: %w", err)
	}

	values := make(map[string]cty.Value)

	for _, path := range append(native, jsonFiles...) {
		data, err := s.Read(path)
		if err != nil {
			return nil, err
		}

		file, err := parseInputFile(path, data)
		if err != nil {
			return nil, err
		}

		content, _, diagnostics := file.Body.PartialContent(&hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
		})
		if diagnostics.HasErrors() {
			slog.Error("tofu.source.variable_read_failed", "err", diagnostics.Error())
			return nil, fmt.Errorf("read installed module variables: %w", diagnostics)
		}

		for _, variable := range content.Blocks {
			value := cty.DynamicVal
			attributes, _, diagnostics := variable.Body.PartialContent(&hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{{Name: "default", Required: false}},
			})
			if diagnostics.HasErrors() {
				slog.Error("tofu.source.variable_default_failed", "err", diagnostics.Error())
				return nil, fmt.Errorf("read installed module defaults: %w", diagnostics)
			}

			if attribute, present := attributes.Attributes["default"]; present {
				candidate, diagnostics := attribute.Expr.Value(nil)
				if !diagnostics.HasErrors() {
					value = candidate
				}
			}

			if supplied, present := inputs[variable.Labels[0]]; present {
				value = supplied
			}

			values[variable.Labels[0]] = value
		}
	}

	return &hcl.EvalContext{Variables: map[string]cty.Value{"var": cty.ObjectVal(values)}}, nil
}

func childModuleInputs(body hcl.Body, context *hcl.EvalContext) (map[string]cty.Value, error) {
	attributes, diagnostics := body.JustAttributes()
	if diagnostics.HasErrors() {
		slog.Error("tofu.source.module_inputs_failed", "err", diagnostics.Error())
		return nil, fmt.Errorf("read installed module inputs: %w", diagnostics)
	}

	inputs := make(map[string]cty.Value)

	for name, attribute := range attributes {
		if name == "source" || name == "version" || name == "providers" || name == "count" ||
			name == "for_each" ||
			name == "depends_on" {
			continue
		}

		value, diagnostics := attribute.Expr.Value(context)
		if diagnostics.HasErrors() {
			value = cty.DynamicVal
		}

		inputs[name] = value
	}

	return inputs, nil
}
