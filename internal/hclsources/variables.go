package hclsources

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/dynblock"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Ask HCL for free traversals at every level. It excludes loop variables in
// expressions and templates, and iterator variables in dynamic blocks.
func freeVariables(body *hclsyntax.Body) []hcl.Traversal {
	return visitVariables(body, dynblock.WalkVariables(body))
}

func visitVariables(body *hclsyntax.Body, node dynblock.WalkVariablesNode) []hcl.Traversal {
	schema := &hcl.BodySchema{}

	for name := range body.Attributes {
		schema.Attributes = append(schema.Attributes, hcl.AttributeSchema{Name: name})
	}

	for _, block := range body.Blocks {
		if block.Type != "dynamic" {
			schema.Blocks = append(
				schema.Blocks,
				hcl.BlockHeaderSchema{Type: block.Type, LabelNames: block.Labels},
			)
		}
	}

	variables, children := node.Visit(schema)
	for _, child := range children {
		// Every child comes from the same native syntax tree as the root body.
		if nested, ok := child.Body().(*hclsyntax.Body); ok {
			variables = append(variables, visitVariables(nested, child.Node)...)
		}
	}

	return variables
}
