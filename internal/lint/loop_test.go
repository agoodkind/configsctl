package lint

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunSparesLoopTargetsInTemplate lints a template whose presence and
// default checks read Jinja for-loop targets, including a tuple target and a
// whitespace-controlled loop header. A loop value is a runtime value, so only
// the check on the input variable is a finding.
func TestRunSparesLoopTargetsInTemplate(t *testing.T) {
	template := filepath.Join(t.TempDir(), "ingress.conf.j2")
	content := "{% for entry in ingress %}{{ entry.hostname is defined }}{% endfor %}\n" +
		"{% for k, v in mapping.items() %}{{ v | default('') }}{% endfor %}\n" +
		"{%- for cidr in pinned -%}{{ cidr is none }}{%- endfor %}\n" +
		"{{ listen_port is defined }}\n"
	if err := os.WriteFile(template, []byte(content), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}

	findings, _, err := Run([]string{template})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %v, want exactly the listen_port presence check", findings)
	}
	if findings[0].Root != "listen_port" || findings[0].Kind != "presence" || findings[0].Line != 4 {
		t.Fatalf("finding = %+v, want presence on listen_port at line 4", findings[0])
	}
}
