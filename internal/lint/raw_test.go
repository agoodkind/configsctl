package lint

import (
	"strings"
	"testing"
)

// TestSpanExprsSkipsRawBlocks checks that the literal braces inside a
// {% raw %} block are not extracted as Jinja, while a real substitution
// between two raw blocks still is. This mirrors a template whose {% raw %}
// block emits a downstream tool's own {{ ... }} syntax verbatim.
func TestSpanExprsSkipsRawBlocks(t *testing.T) {
	content := `bind_addr = "{% raw %}{{ GetAllInterfaces | attr \"address\" }}{% endraw %}` +
		`{{ agent_bind_interface }}{% raw %} tail }}{% endraw %}"`
	exprs := spanExprs(content, 1)
	for _, expr := range exprs {
		if strings.Contains(expr.text, "GetAllInterfaces") || strings.Contains(expr.text, "endraw") {
			t.Errorf("raw-block content must be dropped, got %q", expr.text)
		}
	}
	found := false
	for _, expr := range exprs {
		if strings.TrimSpace(expr.text) == "agent_bind_interface" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected agent_bind_interface extracted from outside raw, got %v", exprs)
	}
}
