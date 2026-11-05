package hclsources

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const protectedRule = "vault reference outside a sensitive variable declaration or reference"

var (
	protectedName  = regexp.MustCompile(`\bvault_[A-Za-z0-9_]+`)
	templateMarker = regexp.MustCompile(`\{[\{%#]`)
)

type installedVariable struct {
	directory string
	name      string
}

type source struct {
	file sourceFile
	data []byte
	body *hclsyntax.Body
	// Each offset identifies the last byte boundary of a single allowed token.
	allowedEnds map[int]bool
}

func parse(file sourceFile) (*source, error) {
	data := []byte(*file.Content)
	if match := templateMarker.FindIndex(data); match != nil {
		line := 1 + bytes.Count(data[:match[0]], []byte("\n"))
		return nil, fmt.Errorf("%s:%d: remove the unresolved template marker", file.Source, line)
	}
	parsed, diagnostics := hclsyntax.ParseConfig(data, file.Source, hcl.InitialPos)
	if diagnostics.HasErrors() {
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == hcl.DiagError {
				return nil, fmt.Errorf("%s: correct the invalid native HCL syntax", diagnostic.Subject)
			}
		}
	}
	body, ok := parsed.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("%s: expected a native HCL body", file.Source)
	}
	return &source{file: file, data: data, body: body, allowedEnds: make(map[int]bool)}, nil
}

func (s *source) declare(declarations map[installedVariable]string, active bool) error {
	for _, block := range s.body.Blocks {
		if block.Type != "variable" || !slices.ContainsFunc(block.Labels, protectedName.MatchString) {
			continue
		}
		if len(block.Labels) != 1 || protectedName.FindString(block.Labels[0]) != block.Labels[0] {
			return fmt.Errorf("%s: use exactly one variable label in the vault namespace", block.DefRange())
		}
		label := block.LabelRanges[0]
		if s.data[label.Start.Byte] != '"' {
			return fmt.Errorf("%s: quote the sensitive variable label", label)
		}
		if !literalTrue(block.Body.Attributes["sensitive"]) || block.Body.Attributes["default"] != nil {
			return fmt.Errorf("%s: declare the variable with sensitive = true and without a default", label)
		}
		if active {
			if isOverride(s.file.Destination) {
				return fmt.Errorf("%s: declare the sensitive variable in a primary file", label)
			}
			key := installedVariable{path.Dir(s.file.Destination), block.Labels[0]}
			if _, exists := declarations[key]; exists {
				return fmt.Errorf("%s: declare the sensitive variable only once per installed module", label)
			}
			declarations[key] = path.Dir(s.file.Source)
		}
		// The literal token ends immediately before the closing quote.
		s.allowedEnds[label.End.Byte-1] = true
	}
	return nil
}

func isOverride(destination string) bool {
	stem := strings.TrimSuffix(path.Base(destination), path.Ext(destination))
	return stem == "override" || strings.HasSuffix(stem, "_override")
}

func literalTrue(attribute *hclsyntax.Attribute) bool {
	if attribute == nil {
		return false
	}
	literal, ok := attribute.Expr.(*hclsyntax.LiteralValueExpr)
	return ok && literal.Val.RawEquals(cty.True)
}

func (s *source) check(declarations map[installedVariable]string) error {
	for _, traversal := range freeVariables(s.body) {
		if len(traversal) < 2 || traversal.RootName() != "var" {
			continue
		}
		attribute, ok := traversal[1].(hcl.TraverseAttr)
		if !ok {
			continue
		}
		key := installedVariable{path.Dir(s.file.Destination), attribute.Name}
		if declarations[key] == path.Dir(s.file.Source) {
			s.allowedEnds[attribute.SrcRange.End.Byte] = true
		}
	}
	// The parser discards comments and folds literal string indexes into traversals.
	// Inspect the native tokens to cover both and decode quoted literals in templates.
	tokens, _ := hclsyntax.LexConfig(s.data, s.file.Source, hcl.InitialPos)
	for _, token := range tokens {
		text := string(token.Bytes)
		if token.Type == hclsyntax.TokenQuotedLit || token.Type == hclsyntax.TokenStringLit {
			var diagnostics hcl.Diagnostics
			text, diagnostics = hclsyntax.ParseStringLiteralToken(token)
			if diagnostics.HasErrors() {
				return fmt.Errorf("%s: correct the invalid HCL string literal", token.Range)
			}
		}
		if protectedName.MatchString(text) && !s.allowedEnds[token.Range.End.Byte] {
			return fmt.Errorf("%s: %s", token.Range, protectedRule)
		}
	}
	return nil
}
