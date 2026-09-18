package main

import (
	"slices"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/ansible"
)

// TestApplyDeployArgTags pins that both --tags forms parse and that repeated
// flags accumulate into DeployOptions.Tags in order.
func TestApplyDeployArgTags(t *testing.T) {
	var opts ansible.DeployOptions

	// `--tags <value>` consumes two tokens.
	consumed, err := applyDeployArg(&opts, []string{"--tags", "isp-lxcs"}, 0)
	if err != nil {
		t.Fatalf("applyDeployArg(--tags isp-lxcs): %v", err)
	}
	if consumed != 2 {
		t.Fatalf("consumed = %d, want 2", consumed)
	}

	// `--tags=<value>` consumes one token and accumulates.
	consumed, err = applyDeployArg(&opts, []string{"--tags=extra"}, 0)
	if err != nil {
		t.Fatalf("applyDeployArg(--tags=extra): %v", err)
	}
	if consumed != 1 {
		t.Fatalf("consumed = %d, want 1", consumed)
	}

	want := []string{"isp-lxcs", "extra"}
	if !slices.Equal(opts.Tags, want) {
		t.Fatalf("opts.Tags = %v, want %v", opts.Tags, want)
	}
}

// TestParseDeployFlags pins the parse half of the deploy flags: both --limit
// forms carry the host through to DeployOptions, and a release flag in either
// form is an unknown argument that stops the deploy. The playbooks pin the
// releases they install, so a parser that swallowed an unknown flag would let
// an operator believe a tag they named was the one deployed.
func TestParseDeployFlags(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantLimit string
		wantErr   string
	}{
		{name: "limit spaced", args: []string{"deploy-mwan", "--limit", "mwan_suburban_servers"}, wantLimit: "mwan_suburban_servers"},
		{name: "limit glued", args: []string{"deploy-mwan", "--limit=mwan_suburban_servers"}, wantLimit: "mwan_suburban_servers"},
		{name: "release spaced", args: []string{"deploy-mwan", "--release", "v1"}, wantErr: "unknown deploy argument"},
		{name: "release glued", args: []string{"deploy-mwan", "--release=v1"}, wantErr: "unknown deploy argument"},
		{name: "opnsensectl release spaced", args: []string{"deploy-opnsense", "--opnsensectl-release", "v1"}, wantErr: "unknown deploy argument"},
		{name: "opnsensectl release glued", args: []string{"deploy-opnsense", "--opnsensectl-release=v1"}, wantErr: "unknown deploy argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseDeploy(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseDeploy(%v) error = %v, want %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDeploy(%v): %v", tc.args, err)
			}
			if opts.Limit != tc.wantLimit {
				t.Fatalf("Limit = %q, want %q", opts.Limit, tc.wantLimit)
			}
		})
	}
}
