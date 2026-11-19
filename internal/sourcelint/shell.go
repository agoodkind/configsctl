package sourcelint

import (
	"path"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"mvdan.cc/sh/v3/syntax"
)

func (rules *sourceRules) shell(command string, location hcl.Range) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		return
	}

	syntax.Walk(
		file,
		func(node syntax.Node) bool {
			switch node := node.(type) {
			case *syntax.Redirect:
				return node.Hdoc == nil
			case *syntax.ForClause:
				if !node.Select && installsFiles(node.Do) {
					rules.fileInstallLoop(location, node.Pos())
				}
			}

			return true
		},
	)
}

func installsFiles(body []*syntax.Stmt) bool {
	for _, statement := range body {
		found := false
		syntax.Walk(
			statement,
			func(node syntax.Node) bool {
				if found {
					return false
				}

				switch node := node.(type) {
				case *syntax.Redirect:
					return node.Hdoc == nil
				case *syntax.CallExpr:
					if len(node.Args) > 1 {
						name := node.Args[0].Lit()
						if path.IsAbs(name) {
							name = path.Base(name)
						}

						found = name == "install" || name == "cp" || name == "mv"
					}
				}

				return !found
			},
		)
		if found {
			return true
		}
	}

	return false
}

func (rules *sourceRules) fileInstallLoop(location hcl.Range, position syntax.Pos) {
	start := location.Start
	start.Line += int(position.Line()) - 1
	start.Byte += int(position.Offset())
	if position.Line() > 1 {
		start.Column = int(position.Col())
	} else {
		start.Column += int(position.Col()) - 1
	}

	rules.findings = append(
		rules.findings,
		sourceFinding{
			location: rules.location(hcl.Range{
				Filename: location.Filename,
				Start:    start,
				End:      start,
			}),
			rule: "file_install_loop",
			message: "warning: review a declared file collection instead of a shell installation loop; " +
				"prove equivalent behavior before replacing it",
			blocking: false,
		},
	)
}
