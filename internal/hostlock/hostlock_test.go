package hostlock_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/hostlock"
)

const (
	firstRun  = "20261005T050000Z-aaaaaaaa"
	secondRun = "20261005T050100Z-bbbbbbbb"
)

func localHost(t *testing.T) hostlock.Host {
	t.Helper()
	return hostlock.Host{Name: "local", User: "", Address: "", Dir: t.TempDir()}
}

func TestSecondRunIsRefusedUntilTheFirstReleases(t *testing.T) {
	host := localHost(t)
	ctx := t.Context()
	if err := hostlock.Do(ctx, host, hostlock.Acquire, firstRun, "controller-a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	err := hostlock.Do(ctx, host, hostlock.Acquire, secondRun, "controller-b")
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Run != firstRun || held.Controller != "controller-a" {
		t.Fatalf("second acquire: err = %v, want the lock of %s on controller-a", err, firstRun)
	}
	if err := hostlock.Do(ctx, host, hostlock.Renew, secondRun, "controller-b"); !errors.Is(err, hostlock.ErrNotHeld) {
		t.Fatalf("renew by the second run: err = %v, want ErrNotHeld", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Release, secondRun, "controller-b"); err != nil {
		t.Fatalf("release by the second run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(host.Dir, "deploy.lock")); err != nil {
		t.Fatalf("a release by another run deleted the lock: %v", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Release, firstRun, "controller-a"); err != nil {
		t.Fatalf("release by the first run: %v", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Acquire, secondRun, "controller-b"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestExpiredLockIsFree(t *testing.T) {
	host := localHost(t)
	stale := firstRun + " controller-a 1\n"
	if err := os.WriteFile(filepath.Join(host.Dir, "deploy.lock"), []byte(stale), 0o600); err != nil {
		t.Fatalf("write the stale lock: %v", err)
	}
	if err := hostlock.Do(t.Context(), host, hostlock.Acquire, secondRun, "controller-b"); err != nil {
		t.Fatalf("acquire over an expired lock: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(host.Dir, "deploy.lock"))
	if err != nil || !strings.HasPrefix(string(body), secondRun+" controller-b ") {
		t.Fatalf("lock file = %q, err = %v; want the second run", body, err)
	}
}

func TestUnlockRequiresTheRunOfTheLock(t *testing.T) {
	host := localHost(t)
	ctx := t.Context()
	if err := hostlock.Do(ctx, host, hostlock.Acquire, firstRun, "controller-a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Unlock, secondRun, "controller-b"); !errors.Is(err, hostlock.ErrNotHeld) {
		t.Fatalf("unlock with another run id: err = %v, want ErrNotHeld", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Unlock, firstRun, "controller-b"); err != nil {
		t.Fatalf("unlock with the run id of the lock: %v", err)
	}
	if err := hostlock.Do(ctx, host, hostlock.Acquire, secondRun, "controller-b"); err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
}

func TestAcquireAllReleasesTakenLocksWhenOneHostIsHeld(t *testing.T) {
	free := localHost(t)
	busy := localHost(t)
	busy.Name = "busy"
	ctx := t.Context()
	if err := hostlock.Do(ctx, busy, hostlock.Acquire, firstRun, "controller-a"); err != nil {
		t.Fatalf("lock the busy host: %v", err)
	}
	_, err := hostlock.AcquireAll(ctx, []hostlock.Host{free, busy}, secondRun, "controller-b")
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Host != "busy" {
		t.Fatalf("AcquireAll: err = %v, want the lock on busy", err)
	}
	if _, err := os.Stat(filepath.Join(free.Dir, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the free host kept a lock after the refusal: %v", err)
	}
}

func TestAcquireAllRefusesTheRunWhenALockCommandFails(t *testing.T) {
	free := localHost(t)
	broken := localHost(t)
	broken.Name = "broken"
	blocker := filepath.Join(broken.Dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	broken.Dir = blocker
	_, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{free, broken}, secondRun, "controller-b")
	if err == nil {
		t.Fatal("AcquireAll: err = nil, want a refusal for the broken host")
	}
	if _, err := os.Stat(filepath.Join(free.Dir, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the free host kept a lock after the refusal: %v", err)
	}
}

func TestRenewAllReportsALockThatAnotherRunTook(t *testing.T) {
	host := localHost(t)
	ctx := t.Context()
	set, err := hostlock.AcquireAll(ctx, []hostlock.Host{host}, firstRun, "controller-a")
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	if err := set.RenewAll(ctx); err != nil {
		t.Fatalf("RenewAll while the run owns the lock: %v", err)
	}
	taken := secondRun + " controller-b 9999999999\n"
	if err := os.WriteFile(filepath.Join(host.Dir, "deploy.lock"), []byte(taken), 0o600); err != nil {
		t.Fatalf("write the lock of the second run: %v", err)
	}
	if err := set.RenewAll(ctx); !errors.Is(err, hostlock.ErrLost) {
		t.Fatalf("RenewAll after another run took the lock: err = %v, want ErrLost", err)
	}
}
