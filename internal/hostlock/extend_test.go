package hostlock_test

import (
	"errors"
	"os"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
)

const windowLease = 30 * time.Second

func TestSecondRunIsRefusedUntilTheDeclaredExpiry(t *testing.T) {
	gateway := newGateway(t)
	ctx := t.Context()
	set := gateway.acquire(t)
	granted := lockExpiry(t, gateway.lockFile, firstRun)
	declared, err := hostlock.Extend(ctx, set.StateDir(), gatewayName, windowLease)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if !declared.After(granted) {
		t.Fatalf("declared expiry = %s, want later than the acquired expiry %s", declared, granted)
	}
	if stored := lockExpiry(t, gateway.lockFile, firstRun); !stored.Equal(declared) {
		t.Fatalf("stored expiry = %s, want the declared %s", stored, declared)
	}
	err = hostlock.Do(ctx, gateway.host, hostlock.Acquire, secondRun, "controller-b")
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Run != firstRun || !held.Expiry.Equal(declared) {
		t.Fatalf("acquire by a second run: err = %v, want the lock of %s until %s", err, firstRun, declared)
	}
	if err := set.RenewAll(ctx); err != nil {
		t.Fatalf("RenewAll after the declaration: %v", err)
	}
	if renewed := lockExpiry(t, gateway.lockFile, firstRun); !renewed.Equal(declared) {
		t.Fatalf("expiry after a renewal = %s, want the declared %s", renewed, declared)
	}
}

func TestExtendRequiresTheRunOfTheLock(t *testing.T) {
	gateway := newGateway(t)
	set := gateway.acquire(t)
	taken := secondRun + " controller-b 9999999999\n"
	if err := os.WriteFile(gateway.lockFile, []byte(taken), 0o600); err != nil {
		t.Fatalf("write the lock of the second run: %v", err)
	}
	_, err := hostlock.Extend(t.Context(), set.StateDir(), gatewayName, windowLease)
	if !errors.Is(err, hostlock.ErrNotHeld) {
		t.Fatalf("Extend on the lock of another run: err = %v, want ErrNotHeld", err)
	}
	body, err := os.ReadFile(gateway.lockFile)
	if err != nil || string(body) != taken {
		t.Fatalf("lock file = %q, err = %v; want %q", body, err, taken)
	}
}

func TestExtendRefusesALeaseAboveTheCap(t *testing.T) {
	gateway := newGateway(t)
	ctx := t.Context()
	set := gateway.acquire(t)
	acquired, err := os.ReadFile(gateway.lockFile)
	if err != nil {
		t.Fatalf("read the lock after the acquire: %v", err)
	}
	_, err = hostlock.Extend(ctx, set.StateDir(), gatewayName, hostlock.MaxExtend+time.Second)
	if !errors.Is(err, hostlock.ErrExtendRefused) {
		t.Fatalf("Extend above the cap: err = %v, want ErrExtendRefused", err)
	}
	body, err := os.ReadFile(gateway.lockFile)
	if err != nil || string(body) != string(acquired) {
		t.Fatalf("lock file = %q, err = %v; want %q", body, err, acquired)
	}
	if _, err := hostlock.Extend(ctx, set.StateDir(), gatewayName, hostlock.MaxExtend); err != nil {
		t.Fatalf("Extend at the cap: %v", err)
	}
}
