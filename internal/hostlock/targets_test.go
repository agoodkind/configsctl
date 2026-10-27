package hostlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/hostlock"
)

func TestLocalInventoryHostLocksThisMachineUntilRelease(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("read the hostname: %v", err)
	}
	inventory := ansible.Inventory{Hosts: map[string]ansible.HostVars{
		hostname: {AnsibleConnection: "local", AnsibleHost: "192.0.2.1", AnsibleUser: "unused"},
	}}
	targets, skipped := hostlock.Targets(inventory, []string{hostname}, hostname)
	if len(skipped) != 0 {
		t.Fatalf("selection excluded the local host: %v", skipped)
	}
	if len(targets) != 1 {
		t.Fatalf("local lock target count = %d, want 1", len(targets))
	}
	host := targets[0]
	if host.Name != hostname || host.Address != "" || host.User != "" || host.Dir != hostlock.DefaultDir {
		t.Fatalf("local lock target = %+v, want this machine with the default lock directory", host)
	}
	host.Dir = t.TempDir()
	ctx := t.Context()
	first, err := hostlock.AcquireAll(ctx, []hostlock.Host{host}, firstRun, "controller-a", hostlock.DefaultLease())
	if err != nil {
		t.Fatalf("acquire the local inventory host: %v", err)
	}
	t.Cleanup(func() { first.ReleaseAll(context.WithoutCancel(ctx)) })
	_, err = hostlock.AcquireAll(ctx, []hostlock.Host{host}, secondRun, "controller-b", hostlock.DefaultLease())
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Host != hostname || held.Run != firstRun {
		t.Fatalf("competing local acquire: err = %v, want the first run on %s", err, hostname)
	}
	first.ReleaseAll(ctx)
	if _, err := os.Stat(filepath.Join(host.Dir, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local lock file after release: err = %v, want a missing file", err)
	}
	second, err := hostlock.AcquireAll(ctx, []hostlock.Host{host}, secondRun, "controller-b", hostlock.DefaultLease())
	if err != nil {
		t.Fatalf("acquire the local inventory host after release: %v", err)
	}
	t.Cleanup(func() { second.ReleaseAll(context.WithoutCancel(ctx)) })
}

func TestTargetsExcludeOtherLocalHostsAndPreserveSSHTargets(t *testing.T) {
	const hostname = "local-host"
	inventory := ansible.Inventory{Hosts: map[string]ansible.HostVars{
		hostname: {AnsibleConnection: "local"},
	}}
	names := []string{hostname}
	for _, name := range []string{"localhost", "controller", "other-" + hostname} {
		if name == hostname {
			continue
		}
		inventory.Hosts[name] = ansible.HostVars{AnsibleConnection: "local", AnsibleHost: hostname}
		names = append(names, name)
	}
	inventory.Hosts["container"] = ansible.HostVars{AnsibleConnection: "docker"}
	names = append(names, "container")
	want := []hostlock.Host{{Name: hostname, User: "", Address: "", Dir: hostlock.DefaultDir}}
	for _, connection := range []string{"", "ssh", "smart"} {
		name := "remote-" + connection
		inventory.Hosts[name] = ansible.HostVars{AnsibleConnection: connection}
		names = append(names, name)
		want = append(want, hostlock.Host{Name: name, User: "root", Address: name, Dir: hostlock.DefaultDir})
	}
	inventory.Hosts["remote-custom"] = ansible.HostVars{AnsibleConnection: "ssh", AnsibleHost: "192.0.2.2", AnsibleUser: "deploy"}
	names = append(names, "remote-custom")
	want = append(want, hostlock.Host{Name: "remote-custom", User: "deploy", Address: "192.0.2.2", Dir: hostlock.DefaultDir})
	targets, skipped := hostlock.Targets(inventory, names, hostname)
	if !slices.Equal(targets, want) {
		t.Fatalf("inventory lock targets = %+v, want %+v", targets, want)
	}
	wantSkipped := []string{"localhost", "controller", "other-" + hostname, "container"}
	if !slices.Equal(skipped, wantSkipped) {
		t.Fatalf("excluded inventory hosts = %v, want %v", skipped, wantSkipped)
	}
}
