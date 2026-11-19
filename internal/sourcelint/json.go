package sourcelint

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	hcljson "github.com/hashicorp/hcl/v2/json"
)

func (rules *sourceRules) readJSON(contents []byte, filename string) error {
	file, diagnostics := hcljson.Parse(contents, filename)
	if diagnostics.HasErrors() {
		return sourceParseError(filename, diagnostics)
	}

	schema := &hcl.BodySchema{
		Attributes: nil,
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "locals", LabelNames: nil},
			{Type: variableBlock, LabelNames: []string{sourceLabelName}},
			{Type: "output", LabelNames: []string{sourceLabelName}},
			{Type: "resource", LabelNames: []string{sourceTypeName, sourceLabelName}},
			{Type: "data", LabelNames: []string{sourceTypeName, sourceLabelName}},
			{Type: moduleBlock, LabelNames: []string{sourceLabelName}},
			{Type: "provider", LabelNames: []string{sourceLabelName}},
		},
	}
	body, _, diagnostics := file.Body.PartialContent(schema)
	if diagnostics.HasErrors() {
		return nil
	}

	for _, block := range body.Blocks {
		attributes, _ := block.Body.JustAttributes()
		for name, attribute := range attributes {
			if skipField(block.Type, name) {
				continue
			}

			if err := rules.jsonExpression(
				attribute.Expr,
				contents,
				filepath.Dir(filename),
				name == provisionerBlock,
				name,
			); err != nil {
				return err
			}
		}
	}

	return nil
}

func (rules *sourceRules) jsonExpression(
	expression hcl.Expression,
	contents []byte,
	module string,
	provisioner bool,
	name string,
) error {
	if provisioner && name == "inline" {
		rules.inline(expression, contents)
	}

	if provisioner && name == "command" {
		rules.shellExpressions([]hcl.Expression{expression}, expression.Range(), contents)
	}

	if provisioner && (name == "script" || name == "scripts") {
		return rules.readScripts(expression, module)
	}

	if pairs, diagnostics := hcl.ExprMap(expression); !diagnostics.HasErrors() {
		for _, pair := range pairs {
			key, diagnostics := pair.Key.Value(nil)
			if diagnostics.HasErrors() {
				continue
			}

			name := key.AsString()
			if err := rules.jsonExpression(
				pair.Value,
				contents,
				module,
				provisioner || name == provisionerBlock,
				name,
			); err != nil {
				return err
			}
		}

		return nil
	}

	if items, diagnostics := hcl.ExprList(expression); !diagnostics.HasErrors() {
		for _, item := range items {
			if err := rules.jsonExpression(item, contents, module, provisioner, name); err != nil {
				return err
			}
		}

		return nil
	}

	template, err := readJSONTemplate(expression, contents)
	if err != nil {
		return err
	}

	if template == nil {
		return nil
	}

	rules.originalLocation = &template.location
	rules.expression(template.expression, module, serviceField(name))
	rules.originalLocation = nil

	return nil
}

type parsedTemplate struct {
	expression hclsyntax.Expression
	location   hcl.Range
}

func readJSONTemplate(expression hcl.Expression, contents []byte) (*parsedTemplate, error) {
	location := expression.Range()
	raw := location.SliceBytes(contents)
	if len(raw) == 0 || raw[0] != '"' {
		return nil, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		slog.Error("tofu.source_lint.json_string_failed", "err", err)

		return nil, fmt.Errorf("decode JSON source string: %w", err)
	}

	template, diagnostics := hclsyntax.ParseTemplate(
		[]byte(text),
		location.Filename,
		location.Start,
	)
	if diagnostics.HasErrors() {
		return nil, nil
	}

	return &parsedTemplate{expression: template, location: location}, nil
}
