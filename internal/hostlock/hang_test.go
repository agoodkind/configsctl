package hostlock_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
)

const (
	hungName     = "hung"
	healthyName  = "healthy"
	attemptSlack = 2 * time.Second
)

func hangLease() hostlock.Lease {
	return hostlock.Lease{TTL: 20 * time.Second, RenewInterval: 4 * time.Second}
}

// holdGuard takes the guard directory of the lock script on host. Each lock
// command on host then waits 30 seconds for the guard before it fails.
func holdGuard(t *testing.T, host hostlock.Host) {
	t.Helper()
	guard := filepath.Join(host.Dir, lockFileName+".guard."+bootID(t))
	if err := os.Mkdir(guard, 0o700); err != nil {
		t.Fatalf("take the guard of %s: %v", host.Name, err)
	}
	t.Cleanup(func() {
		if err := os.Remove(guard); err != nil {
			t.Errorf("give back the guard of %s: %v", host.Name, err)
		}
	})
}

func TestKeepRenewedRenewsOtherHostsWhileOneRenewalHangs(t *testing.T) {
	hung := hostlock.Host{Name: hungName, User: "", Address: "", Dir: t.TempDir()}
	healthy := hostlock.Host{Name: healthyName, User: "", Address: "", Dir: t.TempDir()}
	lease := hangLease()
	ctx := t.Context()
	set, err := hostlock.AcquireAll(ctx, []hostlock.Host{hung, healthy}, firstRun, "controller-a", lease)
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	releaseCtx := context.WithoutCancel(ctx)
	t.Cleanup(func() { set.ReleaseAll(releaseCtx) })
	hungExpiry := lockExpiry(t, filepath.Join(hung.Dir, lockFileName), firstRun)
	holdGuard(t, hung)

	timeout := lease.RenewTimeout()
	started := time.Now()
	err = set.RenewAll(ctx)
	if elapsed := time.Since(started); elapsed > timeout+attemptSlack {
		t.Fatalf("RenewAll with a hung renewal took %s, want the %s attempt limit to end it", elapsed, timeout)
	}
	if err != nil {
		t.Fatalf("RenewAll with a hung renewal before the loss deadline: %v", err)
	}

	report := reportedLoss(t, set)

	if !strings.Contains(report.err.Error(), hungName) {
		t.Fatalf("KeepRenewed reported %v, want the loss of %s", report.err, hungName)
	}
	bound := hungExpiry.Add(-lease.RenewInterval)
	if report.at.Before(bound.Add(-earliestReport)) {
		t.Fatalf("lost lock reported %s before the loss deadline %s of %s", bound.Sub(report.at), bound, hungName)
	}
	healthyExpiry := lockExpiry(t, filepath.Join(healthy.Dir, lockFileName), firstRun)
	if remaining, want := healthyExpiry.Sub(report.at), lease.TTL-2*lease.RenewInterval; remaining < want {
		t.Fatalf("%s lock had %s left at the loss of %s, want at least %s from renewals each %s",
			healthyName, remaining, hungName, want, lease.RenewInterval)
	}
}
