package tofulock_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/configsctl/internal/tofulock"
)

var rootTargets = tofulock.Targets{
	ModuleHosts: map[string]string{"module.vault": "vault", "module.suburban": "suburban"},
	ModuleKeyHosts: map[string]map[string]string{
		"module.overlay": {"poweredge": "poweredge", "suburban": "suburban", "vault": "vault"},
	},
	NodeHosts:  nil,
	GuestHosts: nil,
}

var guestTargets = tofulock.Targets{
	ModuleHosts:    nil,
	ModuleKeyHosts: nil,
	NodeHosts:      map[string]string{"suburban": "suburban", "poweredge": "poweredge", "vault": "vault"},
	GuestHosts:     map[string]string{"suburban/224": "clyde_suburban"},
}

func decideFixture(t *testing.T, name string, targets tofulock.Targets) tofulock.Decision {
	t.Helper()
	plan, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return tofulock.Decide(plan, targets)
}

func requireHosts(t *testing.T, decision tofulock.Decision, want []string) {
	t.Helper()
	if decision.AllHosts {
		t.Fatalf("decision selects every hypervisor (%s), want %v", decision.Reason, want)
	}
	if !slices.Equal(decision.Hosts, want) {
		t.Fatalf("hosts = %v, want %v", decision.Hosts, want)
	}
}

func requireAllHosts(t *testing.T, decision tofulock.Decision) {
	t.Helper()
	if !decision.AllHosts {
		t.Fatalf("hosts = %v, want every hypervisor", decision.Hosts)
	}
	if decision.Reason == "" {
		t.Fatal("decision selects every hypervisor without a reason")
	}
}

func TestOverlayChangeLocksOnlyItsHost(t *testing.T) {
	decision := decideFixture(t, "root_overlay_poweredge.json", rootTargets)
	requireHosts(t, decision, []string{"poweredge"})
}

func TestGuestChangeLocksItsNodeAndGuest(t *testing.T) {
	decision := decideFixture(t, "guest_suburban_224.json", guestTargets)
	requireHosts(t, decision, []string{"clyde_suburban", "suburban"})
}

func TestUnmatchedChangeLocksEveryHypervisor(t *testing.T) {
	decision := decideFixture(t, "root_unmatched.json", rootTargets)
	requireAllHosts(t, decision)
}

func TestKernelModulesReplaceWithUnknownNodeLocksEveryHypervisor(t *testing.T) {
	decision := decideFixture(t, "guest_kernel_modules_replace_unknown_node.json", guestTargets)
	requireAllHosts(t, decision)
}

func TestNoOpAndReadChangesLockNothing(t *testing.T) {
	decision := decideFixture(t, "noop_read_only.json", rootTargets)
	requireHosts(t, decision, []string{})
}
