package sourcelint

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type shellSegment struct {
	text     string
	location hcl.Range
	offset   int
}

func (rules *sourceRules) inline(expression hcl.Expression, contents []byte) {
	items, diagnostics := hcl.ExprList(expression)
	if diagnostics.HasErrors() {
		return
	}

	rules.shellExpressions(items, expression.Range(), contents)
}

func (rules *sourceRules) shellExpressions(
	items []hcl.Expression,
	location hcl.Range,
	contents []byte,
) {
	segments := make([]shellSegment, 0, len(items))
	var command strings.Builder

	for _, item := range items {
		text := "DYNAMIC"
		if template, ok := item.(*hclsyntax.TemplateExpr); ok {
			text = templateText(template)
		} else if contents != nil {
			template, err := readJSONTemplate(item, contents)
			if err == nil && template != nil {
				if template, ok := template.expression.(*hclsyntax.TemplateExpr); ok {
					text = templateText(template)
				}
			}
		}

		segments = append(
			segments,
			shellSegment{text: text, location: item.Range(), offset: command.Len()},
		)
		command.WriteString(text)
		command.WriteByte('\n')
	}

	firstFinding := len(rules.findings)
	rules.shell(
		command.String(),
		hcl.Range{
			Filename: location.Filename,
			Start:    hcl.InitialPos,
			End:      hcl.InitialPos,
		},
	)

	for index := firstFinding; index < len(rules.findings); index++ {
		location := rules.findings[index].location

		for _, segment := range segments {
			if location.Start.Byte < segment.offset ||
				location.Start.Byte > segment.offset+len(segment.text) {
				continue
			}

			mapped := segment.location
			if contents == nil {
				offset := location.Start.Byte - segment.offset
				mapped.Start.Line += strings.Count(segment.text[:offset], "\n")
				mapped.Start.Byte += offset
			}

			rules.findings[index].location = mapped

			break
		}
	}
}
