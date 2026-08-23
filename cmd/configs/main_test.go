package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"goodkind.io/configs/internal/ansible"
	"goodkind.io/configs/internal/release"
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

// TestRunDeployStagesReleaseIntoExtraVars pins the staging contract: the play
// receives the staged tag, commit, and directory as one JSON extra var, after
// any extra vars the operator passed, and the fetch is asked for the tag the
// operator named.
func TestRunDeployStagesReleaseIntoExtraVars(t *testing.T) {
	var fetchedTag string
	fetch := func(_ context.Context, opts release.FetchOptions) (release.Staged, error) {
		fetchedTag = opts.Tag
		if opts.CacheRoot != releaseCacheRoot {
			t.Fatalf("CacheRoot = %q", opts.CacheRoot)
		}
		return release.Staged{
			Tag:           opts.Tag,
			Commit:        "8ce01a27d5f1762e75b3877e766e4c8864f1aa70",
			Dir:           "/stage/" + opts.Tag,
			StackDir:      "/stage/" + opts.Tag + "/wanconfig-stack",
			StackManifest: "/stage/" + opts.Tag + "/wanconfig-stack/manifest.txt",
		}, nil
	}
	var deployed ansible.DeployOptions
	deploy := func(opts ansible.DeployOptions) error {
		deployed = opts
		return nil
	}

	privateTempDir(t)
	err := runDeployWith(cmdEnv{}, []string{"deploy-mwan-failover", "--release", "202608162055-5-8ce01a2", "--extra-var", "x=1"}, fetch, deploy)
	if err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}
	if fetchedTag != "202608162055-5-8ce01a2" {
		t.Fatalf("fetched tag = %q", fetchedTag)
	}
	if len(deployed.ExtraVars) != 2 || deployed.ExtraVars[0] != "x=1" {
		t.Fatalf("ExtraVars = %v", deployed.ExtraVars)
	}
	var vars map[string]string
	if err := json.Unmarshal([]byte(deployed.ExtraVars[1]), &vars); err != nil {
		t.Fatalf("release extra var is not JSON: %v", err)
	}
	want := map[string]string{
		"mwan_release_tag":         "202608162055-5-8ce01a2",
		"mwan_release_commit":      "8ce01a27d5f1762e75b3877e766e4c8864f1aa70",
		"mwan_release_dir":         "/stage/202608162055-5-8ce01a2",
		"wanconfig_stack_dir":      "/stage/202608162055-5-8ce01a2/wanconfig-stack",
		"wanconfig_stack_manifest": "/stage/202608162055-5-8ce01a2/wanconfig-stack/manifest.txt",
	}
	for key, value := range want {
		if vars[key] != value {
			t.Fatalf("%s = %q, want %q", key, vars[key], value)
		}
	}
}

// TestRunDeployDoesNotRunThePlayWhenStagingFails pins that a fetch failure
// stops the deploy before ansible is invoked.
func TestRunDeployDoesNotRunThePlayWhenStagingFails(t *testing.T) {
	fetch := func(_ context.Context, _ release.FetchOptions) (release.Staged, error) {
		return release.Staged{}, errors.New("verify failed")
	}
	deployCalled := false
	deploy := func(_ ansible.DeployOptions) error {
		deployCalled = true
		return nil
	}
	privateTempDir(t)
	err := runDeployWith(cmdEnv{}, []string{"deploy-mwan", "--release", "bad"}, fetch, deploy)
	if err == nil {
		t.Fatal("runDeployWith returned nil, want the staging error")
	}
	if deployCalled {
		t.Fatal("the play ran after staging failed")
	}
}

// TestRunDeployWithoutReleaseSkipsStaging pins that a deploy with no --release
// neither fetches nor adds release vars, so playbooks that do not install mwan
// are unaffected.
func TestRunDeployWithoutReleaseSkipsStaging(t *testing.T) {
	fetch := func(_ context.Context, _ release.FetchOptions) (release.Staged, error) {
		t.Fatal("fetch called without --release")
		return release.Staged{}, nil
	}
	var deployed ansible.DeployOptions
	deploy := func(opts ansible.DeployOptions) error {
		deployed = opts
		return nil
	}
	privateTempDir(t)
	if err := runDeployWith(cmdEnv{}, []string{"deploy-clyde"}, fetch, deploy); err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}
	if len(deployed.ExtraVars) != 0 {
		t.Fatalf("ExtraVars = %v, want none", deployed.ExtraVars)
	}
}

// TestParseDeployRelease pins that both --release forms carry the tag through
// to DeployOptions, so a deploy can stage the named release before the play.
func TestParseDeployRelease(t *testing.T) {
	spaced, err := parseDeploy([]string{"deploy-mwan", "--release", "202608160638-3-03cf29a", "--limit", "mwan_suburban_servers"})
	if err != nil {
		t.Fatalf("parseDeploy: %v", err)
	}
	if spaced.ReleaseTag != "202608160638-3-03cf29a" || spaced.Limit != "mwan_suburban_servers" {
		t.Fatalf("parseDeploy = %+v", spaced)
	}
	glued, err := parseDeploy([]string{"deploy-mwan", "--release=v1.2.3"})
	if err != nil {
		t.Fatalf("parseDeploy: %v", err)
	}
	if glued.ReleaseTag != "v1.2.3" {
		t.Fatalf("ReleaseTag = %q", glued.ReleaseTag)
	}
}
