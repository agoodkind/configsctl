package hostlock_test

import (
	"context"
	"errors"
	"testing"

	"goodkind.io/configsctl/hostlock"
)

func TestAcquireAdditionalRetainsTheHostAndLocksTheSelectedGuest(t *testing.T) {
	host := localHost(t)
	guest, err := hostlock.ForGuest(host, "site", "101")
	if err != nil {
		t.Fatal(err)
	}
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, firstRun, "controller-a", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	defer set.ReleaseAll(context.WithoutCancel(t.Context()))
	stop := set.KeepRenewed(t.Context(), func(error) {})
	defer stop()
	before, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}
	if err := set.AcquireAdditional(t.Context(), []hostlock.Host{host, guest}); err != nil {
		t.Fatal(err)
	}
	if err := before.Check([]string{guest.Name}); !errors.Is(err, hostlock.ErrScope) {
		t.Fatal("the earlier scope accepted a guest that it did not lock")
	}
	after, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}
	if err := after.Check([]string{host.Name, guest.Name}); err != nil {
		t.Fatal(err)
	}
	if err := set.RenewAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, target := range []hostlock.Host{host, guest} {
		_, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{target}, secondRun, "controller-b", hostlock.DefaultLease())
		var held *hostlock.HeldError
		if !errors.As(err, &held) || held.Host != target.Name {
			t.Fatal("another run acquired a host or guest lock from the existing set")
		}
	}
}

func TestAdditionalConflictRetainsTheCredentialHostLock(t *testing.T) {
	host := localHost(t)
	guest, err := hostlock.ForGuest(host, "site", "101")
	if err != nil {
		t.Fatal(err)
	}
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, firstRun, "controller-a", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	defer set.ReleaseAll(context.WithoutCancel(t.Context()))
	other, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{guest}, secondRun, "controller-b", hostlock.DefaultLease())
	if err != nil {
		t.Fatal(err)
	}
	defer other.ReleaseAll(context.WithoutCancel(t.Context()))
	var held *hostlock.HeldError
	if err := set.AcquireAdditional(t.Context(), []hostlock.Host{guest}); !errors.As(err, &held) {
		t.Fatal("the additional acquisition did not refuse another run's guest lock")
	}
	scope, err := set.Scope()
	if err != nil {
		t.Fatal(err)
	}
	if err := scope.Check([]string{host.Name}); err != nil {
		t.Fatal(err)
	}
}
