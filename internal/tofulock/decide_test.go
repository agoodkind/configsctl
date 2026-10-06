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
	NodeHosts:     nil,
	GuestHosts:    nil,
	LockFreeTypes: nil,
}

var guestTargets = tofulock.Targets{
	ModuleHosts:    nil,
	ModuleKeyHosts: nil,
	NodeHosts:      map[string]string{"suburban": "suburban", "poweredge": "poweredge", "vault": "vault"},
	GuestHosts:     map[string]string{"suburban/224": "clyde_suburban"},
	LockFreeTypes:  nil,
}

func guestTargetsWithLockFreeTypes() tofulock.Targets {
	targets := guestTargets
	targets.LockFreeTypes = []string{"mwan_network_config"}
	return targets
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

func TestLockFreeChangeAddsNoHost(t *testing.T) {
	decision := decideFixture(t, "guest_mwan_suburban_224.json", guestTargetsWithLockFreeTypes())
	requireHosts(t, decision, []string{"clyde_suburban", "suburban"})
}

func TestUndeclaredLockFreeTypeLocksEveryHypervisor(t *testing.T) {
	decision := decideFixture(t, "guest_mwan_suburban_224.json", guestTargets)
	requireAllHosts(t, decision)
}

func TestLockFreeChangeWithPoweredgeGuestFileLocksItsNodeAndGuest(t *testing.T) {
	targets := guestTargetsWithLockFreeTypes()
	targets.GuestHosts = map[string]string{"suburban/224": "clyde_suburban", "poweredge/301": "mwan_poweredge"}
	decision := decideFixture(t, "guest_mwan_poweredge_file.json", targets)
	requireHosts(t, decision, []string{"mwan_poweredge", "poweredge"})
}

func TestGuestChangeWithUnknownVmidLocksEveryHypervisor(t *testing.T) {
	decision := decideFixture(t, "guest_vmid_unknown.json", guestTargets)
	requireAllHosts(t, decision)
}

func TestGuestChangeWithNullVmidLocksEveryHypervisor(t *testing.T) {
	decision := decideFixture(t, "guest_vmid_null.json", guestTargets)
	requireAllHosts(t, decision)
}

func TestGuestChangeWithoutVmidLocksOnlyItsNode(t *testing.T) {
	decision := decideFixture(t, "guest_kernel_modules_update.json", guestTargets)
	requireHosts(t, decision, []string{"suburban"})
}

func TestMovedOverlayResourceLocksBothKeys(t *testing.T) {
	decision := decideFixture(t, "root_overlay_moved.json", rootTargets)
	requireHosts(t, decision, []string{"poweredge", "vault"})
}

func TestMovedOverlayResourceFromUndeclaredKeyLocksEveryHypervisor(t *testing.T) {
	targets := rootTargets
	targets.ModuleKeyHosts = map[string]map[string]string{
		"module.overlay": {"suburban": "suburban", "vault": "vault"},
	}
	decision := decideFixture(t, "root_overlay_moved.json", targets)
	requireAllHosts(t, decision)
}

func TestOnlyLockFreeNoOpAndReadChangesLockNothing(t *testing.T) {
	decision := decideFixture(t, "guest_mwan_only.json", guestTargetsWithLockFreeTypes())
	requireHosts(t, decision, []string{})
}
