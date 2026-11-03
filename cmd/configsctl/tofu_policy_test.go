//go:build unix

package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/hostlock"
)

const (
	policyLimitedSettings = "      max_parallelism: 1\n"
	policyHostsSettings   = "      required_hosts:\n        - hv_alpha\n"
	policyInsertBefore    = "      lock_endpoints:\n"
	policyHeldRun         = "20261005T050000Z-aaaaaaaa"
	policyApplyArguments  = "-auto-approve"
)

// policySettings adds workspace policy lines to the lock_targets entry of explicitSettings.
func policySettings(t *testing.T, lines ...string) string {
	t.Helper()
	settings := strings.Replace(explicitSettings, policyInsertBefore, strings.Join(lines, "")+policyInsertBefore, 1)
	if settings == explicitSettings {
		t.Fatal("the fixture settings have no lock_endpoints line to insert the policy before")
	}
	return settings
}

func requireRefusedBeforeMutation(t *testing.T, fixture explicitFixture, result commandResult, want string) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatalf("configsctl ran a command that the workspace policy forbids: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, want) {
		t.Fatalf("the refusal omitted %q: %s", want, result.stderr)
	}
	requireOpenTofuNotStarted(t, result)
	if strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused command took a lock: %s", result.stderr)
	}
	requireNoFile(t, fixture.stateFile())
}

func TestPolicyLimitsParallelismOfThePlanAndTheApply(t *testing.T) {
	fixture := newExplicitFixture(t, policySettings(t, policyLimitedSettings))
	args := []string{policyApplyArguments, "-input=false"}
	requireSuccess(t, fixture.apply(t, args...), args...)
	calls := fixture.calls(t)
	if len(calls) != 3 {
		t.Fatalf("the controller ran %q, want plan, show, apply", calls)
	}
	for _, call := range []string{calls[0], calls[2]} {
		if !strings.Contains(call, " -parallelism=1 ") {
			t.Fatalf("the command ran without the configured parallelism: %q", call)
		}
	}
}

func TestPolicyRefusesParallelismAboveTheLimit(t *testing.T) {
	cases := map[string]struct {
		args     []string
		extraEnv []string
	}{
		"command line":       {[]string{policyApplyArguments, "-parallelism=5"}, nil},
		"TF_CLI_ARGS":        {[]string{policyApplyArguments}, []string{"TF_CLI_ARGS=-parallelism=5"}},
		"TF_CLI_ARGS_apply":  {[]string{policyApplyArguments}, []string{"TF_CLI_ARGS_apply=-parallelism=5"}},
		"lower command line": {[]string{policyApplyArguments, "-parallelism=1"}, []string{"TF_CLI_ARGS=-parallelism=5"}},
		"zero":               {[]string{policyApplyArguments, "-parallelism=0"}, nil},
		"separate value":     {[]string{policyApplyArguments, "-parallelism", "5"}, nil},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, policySettings(t, policyLimitedSettings))
			fixture.extraEnv = test.extraEnv
			result := fixture.apply(t, test.args...)
			requireRefusedBeforeMutation(t, fixture, result, "max_parallelism")
		})
	}
}

func TestPolicyRefusesDisabledBackendLocking(t *testing.T) {
	cases := map[string]struct {
		args     []string
		extraEnv []string
	}{
		"command line": {[]string{policyApplyArguments, "-lock=false"}, nil},
		"TF_CLI_ARGS":  {[]string{policyApplyArguments}, []string{"TF_CLI_ARGS=-lock=false"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, policySettings(t, policyLimitedSettings))
			fixture.extraEnv = test.extraEnv
			result := fixture.apply(t, test.args...)
			requireRefusedBeforeMutation(t, fixture, result, "cannot run with -lock=false")
		})
	}
}

func TestPolicyLocksRequiredHostsForAPlanWithoutChanges(t *testing.T) {
	fixture := newExplicitFixture(t, policySettings(t, policyHostsSettings))
	args := []string{policyApplyArguments, "-input=false"}
	first := fixture.apply(t, args...)
	requireSuccess(t, first, args...)
	if !strings.Contains(first.stderr, "hostlock.acquired") || !strings.Contains(first.stderr, "hosts=2") {
		t.Fatalf("the first apply did not lock the guest and the required host: %s", first.stderr)
	}
	second := fixture.apply(t, args...)
	requireSuccess(t, second, args...)
	if !strings.Contains(second.stderr, "hostlock.acquired") || !strings.Contains(second.stderr, "hosts=1") {
		t.Fatalf("the apply of a plan without changes did not lock the required host: %s", second.stderr)
	}
}

func TestWorkspaceWithoutPolicyLocksNothingForAPlanWithoutChanges(t *testing.T) {
	fixture := newExplicitFixture(t, explicitSettings)
	args := []string{policyApplyArguments, "-input=false"}
	requireSuccess(t, fixture.apply(t, args...), args...)
	second := fixture.apply(t, args...)
	requireSuccess(t, second, args...)
	if !strings.Contains(second.stderr, "hosts=0") {
		t.Fatalf("the apply of a plan without changes locked a host: %s", second.stderr)
	}
}

// holdHostLock takes the lock that every host of the fixture shares. The ssh
// stand-in maps every host to FIXTURE_LOCK_DIR.
func holdHostLock(t *testing.T, fixture explicitFixture) {
	t.Helper()
	host := hostlock.Host{Name: "hv_alpha", User: "", Address: "", Dir: fixture.lockDir}
	if err := hostlock.Do(t.Context(), host, hostlock.Acquire, policyHeldRun, "other-controller"); err != nil {
		t.Fatalf("take the lock of the other run: %v", err)
	}
}

func countApplyCalls(calls []string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "apply ") {
			count++
		}
	}
	return count
}

func TestPolicyBlocksARunWhileAnotherRunHoldsARequiredHost(t *testing.T) {
	fixture := newExplicitFixture(t, policySettings(t, policyHostsSettings))
	args := []string{policyApplyArguments, "-input=false"}
	requireSuccess(t, fixture.apply(t, args...), args...)
	holdHostLock(t, fixture)
	result := fixture.apply(t, args...)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "lock the target hosts") {
		t.Fatalf("the apply ran while another run held the required host: %s", result.stderr)
	}
	if applies := countApplyCalls(fixture.calls(t)); applies != 1 {
		t.Fatalf("OpenTofu ran apply %d times, want only the first apply", applies)
	}
}

func TestWorkspaceWithoutPolicyIgnoresAHeldHostForAPlanWithoutChanges(t *testing.T) {
	fixture := newExplicitFixture(t, explicitSettings)
	args := []string{policyApplyArguments, "-input=false"}
	requireSuccess(t, fixture.apply(t, args...), args...)
	holdHostLock(t, fixture)
	requireSuccess(t, fixture.apply(t, args...), args...)
	if applies := countApplyCalls(fixture.calls(t)); applies != 2 {
		t.Fatalf("OpenTofu ran apply %d times, want 2", applies)
	}
}

func TestPolicyRefusesInvalidSettingsBeforeOpenTofuStarts(t *testing.T) {
	cases := map[string]struct {
		settings string
		want     string
	}{
		"empty required hosts": {policySettings(t, "      required_hosts: []\n"), "required_hosts is empty"},
		"empty required name":  {policySettings(t, "      required_hosts:\n        - \"\"\n"), "required_hosts has an empty host name"},
		"missing endpoint":     {policySettings(t, "      required_hosts:\n        - hv_missing\n"), "lock_endpoints does not declare host hv_missing from required_hosts"},
		"zero parallelism":     {policySettings(t, "      max_parallelism: 0\n"), "max_parallelism is 0"},
		"negative parallelism": {policySettings(t, "      max_parallelism: -2\n"), "max_parallelism is -2"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, test.settings)
			result := fixture.apply(t, policyApplyArguments, "-input=false")
			requireRefusedBeforeMutation(t, fixture, result, test.want)
		})
	}
}

func TestPolicyRefusesStateWritingCommandsWithoutLocks(t *testing.T) {
	cases := map[string][]string{
		"refresh":    {"refresh", "-input=false"},
		"import":     {"import", "terraform_data.item", "id"},
		"state rm":   {"state", "rm", "terraform_data.item"},
		"destroy":    {"destroy", "-auto-approve"},
		"taint":      {"taint", "terraform_data.item"},
		"untaint":    {"untaint", "terraform_data.item"},
		"state mv":   {"state", "mv", "a.b", "c.d"},
		"unlock":     {"force-unlock", "id"},
		"state push": {"state", "push", "state.json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, policySettings(t, policyHostsSettings))
			result := fixture.mutate(t, args...)
			if result.exitCode == 0 {
				t.Fatalf("configsctl ran %v in a workspace that requires locks: %s", args, result.stderr)
			}
			requireOpenTofuNotStarted(t, result)
			if _, err := os.Stat(filepath.Join(fixture.lockDir, "deploy.lock")); err == nil {
				t.Fatal("the refused command left a lock")
			}
		})
	}
}

func TestPolicyRootCannotUseTheInventoryMutationRoute(t *testing.T) {
	withoutGuestModules := strings.Replace(
		policySettings(t, policyHostsSettings, policyLimitedSettings), "  guest_modules:\n    suburban/tack: module.base\n", "", 1)
	cases := map[string]struct {
		settings string
		args     []string
		want     string
	}{
		"guest modules unset": {withoutGuestModules, []string{"apply", policyApplyArguments, "-input=false"}, "tofu.guest_modules is unset"},
		"option before apply": {policySettings(t, policyHostsSettings), []string{"-input=false", "apply", policyApplyArguments}, "the command must start with apply"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newDirectFixture(t, test.settings)
			result := fixture.mutate(t, test.args...)
			requireRefusedBeforeMutation(t, fixture, result, test.want)
		})
	}
}
