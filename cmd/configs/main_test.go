package main

import (
	"slices"
	"testing"

	"goodkind.io/configs/internal/ansible"
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
