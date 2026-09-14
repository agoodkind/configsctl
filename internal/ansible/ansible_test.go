package ansible

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPlaybookArgsTags pins the --tags emission: repeatable tags collapse into a
// single comma-joined `--tags=` argument, and a tag beginning with `-` stays
// glued to the flag so ansible-playbook does not parse it as a separate flag.
func TestPlaybookArgsTags(t *testing.T) {
	tests := []struct {
		name    string
		tags    []string
		wantArg string // expected --tags element, or "" for none
	}{
		{name: "no tags", tags: nil, wantArg: ""},
		{name: "single tag", tags: []string{"isp-lxcs"}, wantArg: "--tags=isp-lxcs"},
		{name: "repeatable tags join", tags: []string{"a", "b"}, wantArg: "--tags=a,b"},
		{name: "leading dash stays glued", tags: []string{"--check"}, wantArg: "--tags=--check"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := playbookArgs(DeployOptions{Playbook: "deploy-x", Tags: tc.tags})
			var got []string
			for _, arg := range args {
				if strings.HasPrefix(arg, "--tags") {
					got = append(got, arg)
				}
			}
			if tc.wantArg == "" {
				if len(got) != 0 {
					t.Fatalf("expected no --tags arg, got %v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected exactly one --tags arg, got %v", got)
			}
			if got[0] != tc.wantArg {
				t.Fatalf("--tags arg = %q, want %q", got[0], tc.wantArg)
			}
		})
	}
}

// TestPlaybookArgsEmitsEveryDeployFlag pins the exact argument vector
// ansible-playbook receives for a deploy that sets every option: each flag sits
// next to its own value, in a stable order, after the vault password file and the
// playbook path.
func TestPlaybookArgsEmitsEveryDeployFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	args := playbookArgs(DeployOptions{
		Playbook:  "deploy-x",
		Limit:     "host1",
		Check:     true,
		Diff:      true,
		ExtraVars: []string{"k=v", `{"a":1}`},
		Tags:      []string{"a", "b"},
	})
	want := []string{
		"--vault-password-file", filepath.Join(home, ".config", "ansible", "vault.pass"),
		"playbooks/deploy-x.yml",
		"--limit", "host1",
		"--check",
		"--diff",
		"--extra-vars", "k=v",
		"--extra-vars", `{"a":1}`,
		"--tags=a,b",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("playbookArgs = %q, want %q", args, want)
	}
}
