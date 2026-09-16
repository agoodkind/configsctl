package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/release"
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

// testCommit is the commit every fake stage reports.
const testCommit = "8ce01a27d5f1762e75b3877e766e4c8864f1aa70"

// recordingFetch stands in for release.Fetch. It stages under
// /stage/<source>/<tag>, the layout release.Fetch uses, sets stack paths only
// for a source with a stack bundle, and records every source and tag it was
// asked for.
func recordingFetch(t *testing.T, fetched *[]release.FetchOptions) releaseFetcher {
	t.Helper()
	return func(_ context.Context, opts release.FetchOptions) (release.Staged, error) {
		if opts.CacheRoot != releaseCacheRoot {
			t.Fatalf("CacheRoot = %q", opts.CacheRoot)
		}
		*fetched = append(*fetched, opts)
		dir := "/stage/" + opts.Source.Name + "/" + opts.Tag
		staged := release.Staged{Tag: opts.Tag, Commit: testCommit, Dir: dir}
		if opts.Source.StackBundle {
			staged.StackDir = dir + "/wanconfig-stack"
			staged.StackManifest = dir + "/wanconfig-stack/manifest.txt"
		}
		return staged, nil
	}
}

// decodeReleaseVars decodes one JSON extra var the deploy command appended.
func decodeReleaseVars(t *testing.T, extraVar string) map[string]string {
	t.Helper()
	var vars map[string]string
	if err := json.Unmarshal([]byte(extraVar), &vars); err != nil {
		t.Fatalf("release extra var %q is not JSON: %v", extraVar, err)
	}
	return vars
}

// wantGatewayVars is the gateway extra var for a tag staged by recordingFetch.
func wantGatewayVars(tag string) map[string]string {
	return map[string]string{
		"mwan_release_tag":         tag,
		"mwan_release_commit":      testCommit,
		"mwan_release_dir":         "/stage/mwan/" + tag,
		"wanconfig_stack_dir":      "/stage/mwan/" + tag + "/wanconfig-stack",
		"wanconfig_stack_manifest": "/stage/mwan/" + tag + "/wanconfig-stack/manifest.txt",
	}
}

// wantOpnsensectlVars is the opnsensectl extra var for a tag staged by
// recordingFetch.
func wantOpnsensectlVars(tag string) map[string]string {
	return map[string]string{
		"opnsensectl_release_tag":    tag,
		"opnsensectl_release_commit": testCommit,
		"opnsensectl_release_dir":    "/stage/opnsensectl/" + tag,
	}
}

// TestRunDeployStagesReleaseIntoExtraVars pins the staging contract: the play
// receives the staged tag, commit, and directory as one JSON extra var per
// release flag, after any extra vars the operator passed, and each fetch is
// asked for the source and tag its flag names. Without --opnsensectl-release
// nothing about opnsensectl is fetched or passed.
func TestRunDeployStagesReleaseIntoExtraVars(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantSources []release.Source
		wantVars    []map[string]string
	}{
		{
			name:        "gateway only",
			args:        []string{"deploy-mwan-failover", "--release", "202608162055-5-8ce01a2", "--extra-var", "x=1"},
			wantSources: []release.Source{release.Gateway},
			wantVars:    []map[string]string{wantGatewayVars("202608162055-5-8ce01a2")},
		},
		{
			name:        "gateway and opnsensectl",
			args:        []string{"deploy-opnsense", "--opnsensectl-release=v0.1.0", "--release", "202608162055-5-8ce01a2", "--extra-var", "x=1"},
			wantSources: []release.Source{release.Gateway, release.Opnsensectl},
			wantVars:    []map[string]string{wantGatewayVars("202608162055-5-8ce01a2"), wantOpnsensectlVars("v0.1.0")},
		},
		{
			name:        "opnsensectl only",
			args:        []string{"deploy-opnsense", "--opnsensectl-release", "v0.1.0", "--extra-var", "x=1"},
			wantSources: []release.Source{release.Opnsensectl},
			wantVars:    []map[string]string{wantOpnsensectlVars("v0.1.0")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fetched []release.FetchOptions
			var deployed ansible.DeployOptions
			deploy := func(opts ansible.DeployOptions) error {
				deployed = opts
				return nil
			}

			privateTempDir(t)
			if err := runDeployWith(cmdEnv{}, tc.args, recordingFetch(t, &fetched), deploy); err != nil {
				t.Fatalf("runDeployWith: %v", err)
			}
			if len(fetched) != len(tc.wantSources) {
				t.Fatalf("fetched %d releases, want %d", len(fetched), len(tc.wantSources))
			}
			for i, want := range tc.wantSources {
				if fetched[i].Source.Name != want.Name || fetched[i].Source.Repo != want.Repo || fetched[i].Source.Binary != want.Binary {
					t.Fatalf("fetch %d source = %+v, want %+v", i, fetched[i].Source, want)
				}
			}
			if len(deployed.ExtraVars) != 1+len(tc.wantVars) || deployed.ExtraVars[0] != "x=1" {
				t.Fatalf("ExtraVars = %v", deployed.ExtraVars)
			}
			for i, want := range tc.wantVars {
				got := decodeReleaseVars(t, deployed.ExtraVars[1+i])
				if !maps.Equal(got, want) {
					t.Fatalf("release extra var %d = %v, want %v", i, got, want)
				}
			}
		})
	}
}

// TestRunDeployDoesNotRunThePlayWhenStagingFails pins that a fetch failure of
// either release stops the deploy before ansible is invoked.
func TestRunDeployDoesNotRunThePlayWhenStagingFails(t *testing.T) {
	for _, failing := range []release.Source{release.Gateway, release.Opnsensectl} {
		t.Run(failing.Name, func(t *testing.T) {
			fetch := func(_ context.Context, opts release.FetchOptions) (release.Staged, error) {
				if opts.Source.Name == failing.Name {
					return release.Staged{}, errors.New("verify failed")
				}
				return release.Staged{Tag: opts.Tag, Commit: testCommit, Dir: "/stage"}, nil
			}
			deployCalled := false
			deploy := func(_ ansible.DeployOptions) error {
				deployCalled = true
				return nil
			}
			privateTempDir(t)
			err := runDeployWith(cmdEnv{}, []string{"deploy-opnsense", "--release", "good", "--opnsensectl-release", "good"}, fetch, deploy)
			if err == nil {
				t.Fatal("runDeployWith returned nil, want the staging error")
			}
			if deployCalled {
				t.Fatal("the play ran after staging failed")
			}
		})
	}
}

// TestRunDeployWithoutReleaseSkipsStaging pins that a deploy with no release
// flag neither fetches nor adds release vars, so playbooks that do not install
// a released binary are unaffected.
func TestRunDeployWithoutReleaseSkipsStaging(t *testing.T) {
	fetch := func(_ context.Context, _ release.FetchOptions) (release.Staged, error) {
		t.Fatal("fetch called without a release flag")
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

// manifestFetch stands in for release.Fetch for a release that carries a
// manifest: the stack paths are empty and every manifest asset is reported
// under Assets, the way release.Fetch reports one.
func manifestFetch(assets map[string]string) releaseFetcher {
	return func(_ context.Context, opts release.FetchOptions) (release.Staged, error) {
		dir := "/stage/" + opts.Source.Name + "/" + opts.Tag
		return release.Staged{Tag: opts.Tag, Commit: testCommit, Dir: dir, Assets: assets}, nil
	}
}

// TestRunDeployHandsManifestAssetsToThePlay pins that a release staged from a
// manifest reaches the play with one variable per asset beside the fixed
// release variables, and without the stack variables the bundle path sets.
func TestRunDeployHandsManifestAssetsToThePlay(t *testing.T) {
	assets := map[string]string{
		"wanconfig_stack_dir": "/stage/mwan/v1/wanconfig-stack",
		"mwan_yang_dir":       "/stage/mwan/v1/yang",
	}
	var deployed ansible.DeployOptions
	deploy := func(opts ansible.DeployOptions) error {
		deployed = opts
		return nil
	}
	privateTempDir(t)
	if err := runDeployWith(cmdEnv{}, []string{"deploy-mwan", "--release", "v1"}, manifestFetch(assets), deploy); err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}
	if len(deployed.ExtraVars) != 1 {
		t.Fatalf("ExtraVars = %v, want one release var", deployed.ExtraVars)
	}
	want := map[string]string{
		"mwan_release_tag":    "v1",
		"mwan_release_commit": testCommit,
		"mwan_release_dir":    "/stage/mwan/v1",
		"wanconfig_stack_dir": "/stage/mwan/v1/wanconfig-stack",
		"mwan_yang_dir":       "/stage/mwan/v1/yang",
	}
	if got := decodeReleaseVars(t, deployed.ExtraVars[0]); !maps.Equal(got, want) {
		t.Fatalf("release extra var = %v, want %v", got, want)
	}
}

// TestRunDeployRefusesAManifestVariableThatCollides pins that a manifest
// entry naming one of the fixed release variables stops the deploy before
// the play, so a manifest can never redirect the binary directory.
func TestRunDeployRefusesAManifestVariableThatCollides(t *testing.T) {
	for _, name := range []string{"mwan_release_dir", "mwan_release_commit", "mwan_release_tag"} {
		t.Run(name, func(t *testing.T) {
			deployCalled := false
			deploy := func(_ ansible.DeployOptions) error {
				deployCalled = true
				return nil
			}
			privateTempDir(t)
			err := runDeployWith(cmdEnv{}, []string{"deploy-mwan", "--release", "v1"}, manifestFetch(map[string]string{name: "/elsewhere"}), deploy)
			if err == nil {
				t.Fatal("runDeployWith returned nil, want the collision refused")
			}
			if deployCalled {
				t.Fatal("the play ran after a manifest variable collided")
			}
		})
	}
}

// TestParseDeployRelease pins that both forms of each release flag carry the
// tag through to DeployOptions, so a deploy can stage the named release before
// the play.
func TestParseDeployRelease(t *testing.T) {
	spaced, err := parseDeploy([]string{"deploy-mwan", "--release", "202608160638-3-03cf29a", "--opnsensectl-release", "v0.1.0", "--limit", "mwan_suburban_servers"})
	if err != nil {
		t.Fatalf("parseDeploy: %v", err)
	}
	if spaced.ReleaseTag != "202608160638-3-03cf29a" || spaced.OpnsensectlReleaseTag != "v0.1.0" || spaced.Limit != "mwan_suburban_servers" {
		t.Fatalf("parseDeploy = %+v", spaced)
	}
	glued, err := parseDeploy([]string{"deploy-mwan", "--release=v1.2.3", "--opnsensectl-release=v0.2.0"})
	if err != nil {
		t.Fatalf("parseDeploy: %v", err)
	}
	if glued.ReleaseTag != "v1.2.3" || glued.OpnsensectlReleaseTag != "v0.2.0" {
		t.Fatalf("ReleaseTag = %q, OpnsensectlReleaseTag = %q", glued.ReleaseTag, glued.OpnsensectlReleaseTag)
	}
}
