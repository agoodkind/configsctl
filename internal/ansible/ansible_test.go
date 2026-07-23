package ansible

import (
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

// TestPlaybookArgsTagsCoexist checks --tags does not disturb --limit and
// --extra-vars emission.
func TestPlaybookArgsTagsCoexist(t *testing.T) {
	args := playbookArgs(DeployOptions{
		Playbook:  "deploy-x",
		Limit:     "host1",
		ExtraVars: []string{"k=v"},
		Tags:      []string{"a", "b"},
	})
	if !slices.Contains(args, "--tags=a,b") {
		t.Fatalf("missing --tags=a,b in %v", args)
	}
	if !slices.Contains(args, "--limit") || !slices.Contains(args, "host1") {
		t.Fatalf("missing --limit host1 in %v", args)
	}
	if !slices.Contains(args, "--extra-vars") || !slices.Contains(args, "k=v") {
		t.Fatalf("missing --extra-vars k=v in %v", args)
	}
}
