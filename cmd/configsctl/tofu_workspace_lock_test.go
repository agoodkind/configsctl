package main_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/tofulock"
)

const independentLockSettings = `tofu:
  workspaces_dir: workspaces
  env_key_prefix: fixture_env_
  guest_modules:
    suburban/tack: module.base
  lock_targets:
    workspaces/suburban/tack:
      node_hosts:
        site: hv_alpha
      guest_hosts:
        site/101: guest-alpha
    workspaces/suburban/host:
      module_hosts:
        module.host: hv_alpha
    workspaces/suburban/firmware:
      module_hosts:
        module.host: hv_alpha
`

const independentLockInventory = `all:
  hosts:
    guest-alpha:
      ansible_connection: local
      service_name: guest_alpha
  children:
    proxmox_servers:
      hosts:
        hv_alpha:
          ansible_connection: local
        hv_beta:
          ansible_connection: local
`

func independentLockTree(t *testing.T) configsTree {
	t.Helper()
	tree := newGatedTree(t, true)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), independentLockSettings)
	writeFixtureFile(t, filepath.Join(tree.root, "ansible", "inventory", "hosts.yml"), independentLockInventory)
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "tack.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "gate.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), gatedItemModule)
	args := tofuArgs("suburban/tack", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	return tree
}

func requireWorkspaceLockRefusal(t *testing.T, tree configsTree, result commandResult, host string) {
	t.Helper()
	if result.exitCode == 0 {
		t.Fatal("the apply did not require its declared SSH lock")
	}
	for _, fragment := range []string{"hosts=[" + host + "]", "host " + host + " from tofu.lock_targets has no ssh lock target"} {
		if !strings.Contains(result.stderr, fragment) {
			t.Fatalf("the lock refusal omitted %q: %s", fragment, result.stderr)
		}
	}
	if strings.Contains(result.stderr, "tofu.lock.all_hypervisors") || strings.Contains(result.stderr, "hostlock.acquired") {
		t.Fatalf("the refused apply selected extra locks: %s", result.stderr)
	}
	requireNoPlanCopies(t, tree)
}

func TestIndependentApplyLocksOnlyItsDeclaredGuest(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			tree := independentLockTree(t)
			args := tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false", "-target=module.base")
			if saved {
				plan := tofuArgs("suburban/tack", "plan", "-input=false", "-target=module.base", "-out=guest.plan")
				requireSuccess(t, runConfigsctl(t, tree, plan...), plan...)
				args = tofuArgs("suburban/tack", "apply", "-input=false", "guest.plan")
			}
			requireWorkspaceLockRefusal(t, tree, runConfigsctl(t, tree, args...), "guest-alpha")
			requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
		})
	}
}

func TestIndependentHostAndFirmwareApplyRequireTheirPhysicalHost(t *testing.T) {
	for _, leaf := range []string{"host", "firmware"} {
		t.Run(leaf, func(t *testing.T) {
			tree := independentLockTree(t)
			selector := "suburban/" + leaf
			root := filepath.Join(tree.workspaces, "suburban", leaf)
			writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "host.tfstate"))
			writeFixtureFile(t, filepath.Join(root, "main.tf"), "module \"host\" {\n  source = \"./item\"\n  value = \"host\"\n}\n")
			writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), lockedItemModule)
			init := tofuArgs(selector, "init", "-input=false")
			requireSuccess(t, runConfigsctl(t, tree, init...), init...)
			args := tofuArgs(selector, "apply", "-auto-approve", "-input=false")
			requireWorkspaceLockRefusal(t, tree, runConfigsctl(t, tree, args...), "hv_alpha")
			requireNoFile(t, filepath.Join(root, "host.tfstate"))
			planArgs := tofuArgs(selector, "plan", "-input=false", "-out=host.plan")
			requireSuccess(t, runConfigsctl(t, tree, planArgs...), planArgs...)
			show := runConfigsctl(t, tree, tofuArgs(selector, "show", "-json", "host.plan")...)
			if show.exitCode != 0 {
				t.Fatal("the native host plan could not be read")
			}
			targets := tofulock.Targets{ModuleHosts: map[string]string{"module.host": "hv_alpha"}, ModuleKeyHosts: nil, NodeHosts: nil, GuestHosts: nil, LockFreeTypes: nil}
			decision := tofulock.DecideIndependent([]byte(show.stdout), targets)
			requireRealPlanLock(t, decision, "hv_alpha")
		})
	}
}

func TestIndependentApplyRefusesUnknownOwnership(t *testing.T) {
	tree := independentLockTree(t)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "unowned.tf"), "resource \"terraform_data\" \"unowned\" {\n  input = \"unowned\"\n}\n")
	args := tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false", "-target=terraform_data.unowned")
	result := runConfigsctl(t, tree, args...)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "unknown lock ownership") || !strings.Contains(result.stderr, "terraform_data.unowned matches no lock target") {
		t.Fatalf("the independent apply did not refuse unknown ownership: %s", result.stderr)
	}
	if strings.Contains(result.stderr, "hostlock.acquired") || strings.Contains(result.stderr, "tofu.lock.all_hypervisors") {
		t.Fatalf("the unknown resource acquired locks: %s", result.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
}

func independentUnlockedTree(t *testing.T) configsTree {
	t.Helper()
	tree := independentLockTree(t)
	configuration := strings.Replace(independentLockSettings, "    workspaces/suburban/tack:\n", "    workspaces/suburban/tack:\n      lock_free_types: [terraform_data]\n", 1)
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), configuration)
	return tree
}

func runMutationInput(t *testing.T, tree configsTree, input string, args ...string) commandResult {
	t.Helper()
	tempDir, err := os.MkdirTemp(shortTempRoot, "workspace-lock-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tempDir) })
	command := exec.CommandContext(t.Context(), configsctlBinary, args...)
	command.Dir = tree.root
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tree.home, "TMPDIR=" + tempDir}
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), exitCode: 0}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.exitCode = exited.ExitCode()
	} else if err != nil {
		t.Fatalf("run a mutation with confirmation input: %v", err)
	}
	appendFailedRunLogs(t, &result, tempDir)
	return result
}

func TestIndependentApplyRetainsConfirmationAndDestroy(t *testing.T) {
	tree := independentUnlockedTree(t)
	args := tofuArgs("suburban/tack", "apply", "-auto-approve=false")
	refused := runMutationInput(t, tree, "cancel\n", args...)
	if refused.exitCode == 0 || !strings.Contains(refused.stderr, "apply canceled") {
		t.Fatalf("the apply ignored the canceled confirmation: %s", refused.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
	accepted := runMutationInput(t, tree, "yes\n", args...)
	requireSuccess(t, accepted, args...)
	requireFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
	destroy := tofuArgs("suburban/tack", "destroy", "-auto-approve", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, destroy...), destroy...)
	list := tofuArgs("suburban/tack", "state", "list")
	result := runConfigsctl(t, tree, list...)
	requireSuccess(t, result, list...)
	if strings.TrimSpace(result.stdout) != "" {
		t.Fatal("the confirmed destroy retained managed resources")
	}
}

func TestIndependentApplyRefusesNoninteractiveApproval(t *testing.T) {
	tree := independentUnlockedTree(t)
	args := tofuArgs("suburban/tack", "apply", "-auto-approve=false", "-input=false")
	result := runConfigsctl(t, tree, args...)
	if result.exitCode == 0 || !strings.Contains(result.stderr, "requires -auto-approve") {
		t.Fatalf("the noninteractive apply bypassed confirmation: %s", result.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
}

func TestIndependentApplyHelpDoesNotPlanOrLock(t *testing.T) {
	tree := independentLockTree(t)
	args := tofuArgs("suburban/tack", "apply", "-help")
	result := runConfigsctl(t, tree, args...)
	requireSuccess(t, result, args...)
	if !strings.Contains(result.stdout, "Usage: tofu") || strings.Contains(result.stderr, "tofu.lock.") {
		t.Fatalf("the mutation help command planned or selected locks: %s", result.stderr)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
}

func TestIndependentLockFreeWorkspaceNeedsNoHostMapping(t *testing.T) {
	tree := independentLockTree(t)
	configuration := independentLockSettings + "    workspaces/shared:\n      lock_free_types: [terraform_data]\n"
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), configuration)
	root := filepath.Join(tree.workspaces, "shared")
	writeFixtureFile(t, filepath.Join(root, "backend.tf"), fmt.Sprintf(localBackend, "shared.tfstate"))
	writeFixtureFile(t, filepath.Join(root, "main.tf"), "resource \"terraform_data\" \"shared\" {\n  input = \"shared-fixture\"\n}\n")
	init := tofuArgs("shared", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, init...), init...)
	apply := tofuArgs("shared", "apply", "-auto-approve", "-input=false")
	result := runConfigsctl(t, tree, apply...)
	requireSuccess(t, result, apply...)
	if strings.Contains(result.stderr, "tofu.lock.all_hypervisors") || !strings.Contains(result.stderr, "hosts=[]") {
		t.Fatalf("the lock-free workspace selected host locks: %s", result.stderr)
	}
	requireFile(t, filepath.Join(root, "shared.tfstate"))
}

func TestIndependentGuestPlanUsesARealGuestLock(t *testing.T) {
	tree := independentLockTree(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	plugin := filepath.Join(home, ".terraform.d", "plugins", "tofu.home.arpa", "agoodkind", "pveguest", "0.1.0", platform, "terraform-provider-pveguest_v0.1.0")
	if _, err := os.Stat(plugin); errors.Is(err, os.ErrNotExist) {
		t.Skip("the real pveguest provider must be installed in the local OpenTofu mirror")
	} else if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(tree.home, ".terraform.d", "plugins", "tofu.home.arpa", "agoodkind", "pveguest", "0.1.0", platform)
	if err := os.MkdirAll(mirror, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plugin, filepath.Join(mirror, filepath.Base(plugin))); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "main.tf"), workspaceFixture(t, "lock-guest.tf"))
	writeFixtureFile(t, filepath.Join(root, "item", "main.tf"), workspaceFixture(t, "lock-guest-item.tf"))
	init := tofuArgs("suburban/tack", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, init...), init...)
	planArgs := tofuArgs("suburban/tack", "plan", "-input=false", "-refresh=false", "-out=guest.plan")
	requireSuccess(t, runMutationInput(t, tree, "", planArgs...), planArgs...)
	showArgs := tofuArgs("suburban/tack", "show", "-json", "guest.plan")
	shown := runMutationInput(t, tree, "", showArgs...)
	if shown.exitCode != 0 {
		t.Fatal("the real guest plan could not be read")
	}
	targets := tofulock.Targets{GuestHosts: map[string]string{"site/101": "guest-alpha"}, NodeHosts: map[string]string{"site": "hv_alpha"}, ModuleHosts: nil, ModuleKeyHosts: nil, LockFreeTypes: nil}
	decision := tofulock.DecideIndependent([]byte(shown.stdout), targets)
	if decision.AllHosts || !slices.Equal(decision.Hosts, []string{"guest-alpha"}) {
		t.Fatalf("the real guest plan selected %v: %s", decision.Hosts, decision.Reason)
	}
	requireRealPlanLock(t, decision, "guest-alpha")
	targets.ModuleHosts = map[string]string{`module.base["guest_alpha"].pveguest_file.item`: "hv_alpha"}
	requireRealPlanLock(t, tofulock.DecideIndependent([]byte(shown.stdout), targets), "hv_alpha")
	targets.ModuleHosts = nil
	targets.GuestHosts = nil
	guestRoute := tofulock.DecideIndependent([]byte(shown.stdout), targets)
	if guestRoute.AllHosts || len(guestRoute.Hosts) != 0 || len(guestRoute.GuestLocks) != 1 {
		t.Fatalf("the unenrolled guest did not select its node/VMID lock: %s", guestRoute.Reason)
	}
	requireRealNodeGuestLock(t, guestRoute.GuestLocks[0])
	targets.NodeHosts = nil
	if unknown := tofulock.DecideIndependent([]byte(shown.stdout), targets); !unknown.AllHosts {
		t.Fatal("the real guest plan accepted a missing node owner")
	}
	apply := tofuArgs("suburban/tack", "apply", "-input=false", "guest.plan")
	requireWorkspaceLockRefusal(t, tree, runMutationInput(t, tree, "", apply...), "guest-alpha")
	configuration := independentLockSettings + "    workspaces/suburban/simulator:\n      node_hosts:\n        site: hv_alpha\n"
	writeFixtureFile(t, filepath.Join(tree.root, settingsName), configuration)
	simulator := filepath.Join(tree.workspaces, "suburban", "simulator")
	writeFixtureFile(t, filepath.Join(simulator, "backend.tf"), fmt.Sprintf(localBackend, "simulator.tfstate"))
	writeFixtureFile(t, filepath.Join(simulator, "main.tf"), workspaceFixture(t, "lock-guest.tf"))
	writeFixtureFile(t, filepath.Join(simulator, "item", "main.tf"), workspaceFixture(t, "lock-guest-item.tf"))
	init = tofuArgs("suburban/simulator", "init", "-input=false")
	requireSuccess(t, runConfigsctl(t, tree, init...), init...)
	apply = tofuArgs("suburban/simulator", "apply", "-auto-approve", "-input=false", "-refresh=false")
	routed := runMutationInput(t, tree, "", apply...)
	if routed.exitCode == 0 || !strings.Contains(routed.stderr, "hosts=[] guest_locks=\"[{Node:site VMID:101 Host:hv_alpha}]\"") || !strings.Contains(routed.stderr, "host hv_alpha from tofu.lock_targets has no ssh lock target") {
		t.Fatalf("the simulator apply did not require its guest lock through hv_alpha: %s", routed.stderr)
	}
	if strings.Contains(routed.stderr, "tofu.lock.all_hypervisors") {
		t.Fatalf("the simulator apply selected a global host lock: %s", routed.stderr)
	}
	requireNoFile(t, filepath.Join(simulator, "simulator.tfstate"))
}

func TestIndependentApplyPreservesVariableFlags(t *testing.T) {
	for _, flags := range [][]string{{"-var", "healthy=false"}, {"-var-file", "unhealthy.tfvars"}} {
		t.Run(flags[0], func(t *testing.T) {
			tree := independentUnlockedTree(t)
			root := filepath.Join(tree.workspaces, "suburban", "tack")
			configuration := "variable \"healthy\" {\n  type = bool\n  default = true\n}\n" + strings.Replace(workspaceFixture(t, "gate.tf"), "healthy = true", "healthy = var.healthy", 1)
			writeFixtureFile(t, filepath.Join(root, "main.tf"), configuration)
			writeFixtureFile(t, filepath.Join(root, "unhealthy.tfvars"), "healthy = false\n")
			args := append(tofuArgs("suburban/tack", "apply", "-auto-approve", "-input=false"), flags...)
			result := runMutationInput(t, tree, "", args...)
			if result.exitCode == 0 || !strings.Contains(result.stderr, "fixture precondition failed for guest_alpha") {
				t.Fatalf("the native mutation plan did not use its variable flags: %s", result.stderr)
			}
			requireNoFile(t, filepath.Join(root, "tack.tfstate"))
		})
	}
}

func TestIndependentApplyCancelsDuringConfirmation(t *testing.T) {
	tree := independentUnlockedTree(t)
	output, err := os.CreateTemp(t.TempDir(), "confirmation-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	command := exec.CommandContext(ctx, configsctlBinary, tofuArgs("suburban/tack", "apply")...)
	command.Dir = tree.root
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tree.home, "TMPDIR=" + filepath.Join(tree.root, "tmp")}
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		contents, err := os.ReadFile(output.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "Enter a value:") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("the mutation did not display its confirmation prompt")
		case <-ticker.C:
		}
	}
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 1 {
		t.Fatalf("the interrupted confirmation returned %v", err)
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "suburban", "tack", "tack.tfstate"))
	leftovers, err := filepath.Glob(filepath.Join(tree.root, "tmp", "configsctl-mutation-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("the canceled mutation retained private plans: %v, %v", leftovers, err)
	}
}

func requireRealNodeGuestLock(t *testing.T, guest tofulock.GuestLock) {
	t.Helper()
	base := t.TempDir()
	node := hostlock.Host{Name: guest.Host, User: "", Address: "", Dir: base}
	host, err := hostlock.ForGuest(node, guest.Node, guest.VMID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, lockRun, "first", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.ReleaseAll(context.WithoutCancel(t.Context())) })
	requireFile(t, filepath.Join(base, "guests", guest.VMID, "deploy.lock"))
	requireNoFile(t, filepath.Join(base, "deploy.lock"))
	otherGuest, err := hostlock.ForGuest(node, guest.Node, "102")
	if err != nil {
		t.Fatal(err)
	}
	other, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{otherGuest}, otherLockRun, "other-guest", hostlock.DefaultLease())
	if err != nil {
		t.Fatalf("a different guest could not acquire its own lock: %v", err)
	}
	other.ReleaseAll(context.WithoutCancel(t.Context()))
	_, err = hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, otherLockRun, "same-guest", hostlock.DefaultLease())
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Host != host.Name {
		t.Fatalf("the node/VMID lock did not refuse a conflicting operation: %v", err)
	}
	first.ReleaseAll(context.WithoutCancel(t.Context()))
	second, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, otherLockRun, "same-guest", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	second.ReleaseAll(context.WithoutCancel(t.Context()))
}

func requireRealPlanLock(t *testing.T, decision tofulock.Decision, name string) {
	t.Helper()
	if decision.AllHosts || !slices.Equal(decision.Hosts, []string{name}) {
		t.Fatalf("the native plan selected %v: %s", decision.Hosts, decision.Reason)
	}
	host := hostlock.Host{Name: name, User: "", Address: "", Dir: t.TempDir()}
	first, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, lockRun, "first", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.ReleaseAll(context.WithoutCancel(t.Context())) })
	_, err = hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, otherLockRun, "second", hostlock.DefaultLease())
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Host != name {
		t.Fatalf("a conflicting operation did not refuse the acquired lock on %s: %v", name, err)
	}
	first.ReleaseAll(context.WithoutCancel(t.Context()))
	second, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, otherLockRun, "second", hostlock.DefaultLease())
	if err != nil {
		t.Fatalf("the lock on %s remained unavailable after release: %v", name, err)
	}
	second.ReleaseAll(context.WithoutCancel(t.Context()))
}
