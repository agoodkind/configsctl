package lint

import "testing"

// TestCollectLoopVars checks that Jinja for-loop targets, including tuple
// targets and whitespace-control markers, are gathered as runtime names.
func TestCollectLoopVars(t *testing.T) {
	content := "{% for entry in ingress %}\n" +
		"{% for k, v in mapping.items() %}\n" +
		"{%- for cidr in pinned -%}\n"
	runtime := map[string]struct{}{}
	collectLoopVars(content, runtime)
	for _, want := range []string{"entry", "k", "v", "cidr"} {
		if _, ok := runtime[want]; !ok {
			t.Errorf("expected loop target %q to be collected as runtime", want)
		}
	}
}

// TestLoopVarPresenceSpared confirms a presence check on a for-loop target is
// spared, since a loop value is a runtime value the doctrine allows.
func TestLoopVarPresenceSpared(t *testing.T) {
	runtime := map[string]struct{}{}
	collectLoopVars("{% for entry in ingress %}", runtime)
	constructs, _ := Analyze("{{ entry.hostname is defined }}")
	for _, construct := range constructs {
		if IsViolation(construct, runtime) {
			t.Errorf("loop target entry should be spared, got violation %+v", construct)
		}
	}
}
