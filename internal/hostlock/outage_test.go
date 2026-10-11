package hostlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
)

const (
	gatewayName     = "gateway"
	declaredLease   = 12 * time.Second
	shortOutage     = 5 * time.Second
	reportSlack     = 500 * time.Millisecond
	reportWait      = 20 * time.Second
	earliestReport  = 2 * time.Second
	lockFileName    = "deploy.lock"
	offlineContents = "offline\n"
)

func shortLease() hostlock.Lease {
	return hostlock.Lease{TTL: 9 * time.Second, RenewInterval: 4 * time.Second}
}

type gateway struct {
	host       hostlock.Host
	offlineDir string
	lockFile   string
}

func newGateway(t *testing.T) gateway {
	t.Helper()
	lockDir := filepath.Join(t.TempDir(), "lock")
	return gateway{
		host:       hostlock.Host{Name: gatewayName, User: "", Address: "", Dir: lockDir},
		offlineDir: lockDir + ".offline",
		lockFile:   filepath.Join(lockDir, lockFileName),
	}
}

func (g gateway) acquire(t *testing.T) *hostlock.Set {
	t.Helper()
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{g.host}, firstRun, "controller-a", shortLease())
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	releaseCtx := context.WithoutCancel(t.Context())
	t.Cleanup(func() { set.ReleaseAll(releaseCtx) })
	return set
}

func (g gateway) goOffline(t *testing.T) {
	t.Helper()
	if err := os.Rename(g.host.Dir, g.offlineDir); err != nil {
		t.Fatalf("take the host offline: %v", err)
	}
	if err := os.WriteFile(g.host.Dir, []byte(offlineContents), 0o600); err != nil {
		t.Fatalf("block the lock directory: %v", err)
	}
}

func (g gateway) comeBack(t *testing.T) {
	t.Helper()
	if err := os.Remove(g.host.Dir); err != nil {
		t.Fatalf("unblock the lock directory: %v", err)
	}
	if err := os.Rename(g.offlineDir, g.host.Dir); err != nil {
		t.Fatalf("bring the host back: %v", err)
	}
}

func lockExpiry(t *testing.T, lockFile, run string) time.Time {
	t.Helper()
	body, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatalf("read the lock: %v", err)
	}
	fields := strings.Fields(string(body))
	if len(fields) != 3 || fields[0] != run {
		t.Fatalf("lock file = %q, want a lock of %s", body, run)
	}
	seconds, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		t.Fatalf("lock file = %q, want an expiry in epoch seconds: %v", body, err)
	}
	return time.Unix(seconds, 0)
}

func waitFor(t *testing.T, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-t.Context().Done():
		t.Fatalf("the test context ended during a wait of %s", duration)
	case <-timer.C:
	}
}

type lossReport struct {
	err error
	at  time.Time
}

func reportedLoss(t *testing.T, set *hostlock.Set) lossReport {
	t.Helper()
	reports := make(chan lossReport, 1)
	stop := set.KeepRenewed(t.Context(), func(err error) {
		reports <- lossReport{err: err, at: time.Now()}
	})
	defer stop()
	timer := time.NewTimer(reportWait)
	defer timer.Stop()
	select {
	case report := <-reports:
		if !errors.Is(report.err, hostlock.ErrLost) {
			t.Fatalf("KeepRenewed reported %v, want ErrLost", report.err)
		}
		return report
	case <-timer.C:
		t.Fatalf("KeepRenewed reported no lost lock in %s", reportWait)
	}
	return lossReport{err: nil, at: time.Time{}}
}

func TestRenewAllKeepsADeclaredLeaseThroughAnOutage(t *testing.T) {
	gateway := newGateway(t)
	ctx := t.Context()
	set := gateway.acquire(t)
	declared, err := hostlock.Extend(ctx, set.StateDir(), gatewayName, declaredLease)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}

	gateway.goOffline(t)
	waitFor(t, shortOutage)
	duringOutage := set.RenewAll(ctx)
	gateway.comeBack(t)

	if errors.Is(duringOutage, hostlock.ErrLost) {
		t.Fatalf("RenewAll %s into a declared outage of %s: err = %v, want no ErrLost",
			shortOutage, declaredLease, duringOutage)
	}
	if err := set.RenewAll(ctx); err != nil {
		t.Fatalf("RenewAll after the host returned: %v", err)
	}
	if renewed := lockExpiry(t, gateway.lockFile, firstRun); renewed.Before(declared) {
		t.Fatalf("expiry after the renewal = %s, want no earlier than the declared %s", renewed, declared)
	}
	err = hostlock.Do(ctx, gateway.host, hostlock.Acquire, secondRun, "controller-b")
	var held *hostlock.HeldError
	if !errors.As(err, &held) || held.Run != firstRun {
		t.Fatalf("acquire by a second run after the outage: err = %v, want the lock of %s", err, firstRun)
	}
}

func TestKeepRenewedReportsAnUndeclaredOutageOneIntervalBeforeExpiry(t *testing.T) {
	gateway := newGateway(t)
	set := gateway.acquire(t)
	expiry := lockExpiry(t, gateway.lockFile, firstRun)
	gateway.goOffline(t)

	report := reportedLoss(t, set)

	bound := expiry.Add(-shortLease().RenewInterval)
	if report.at.After(bound.Add(reportSlack)) {
		t.Fatalf("lost lock reported %s after %s, want a report one renewal interval before the expiry %s",
			report.at.Sub(bound), bound, expiry)
	}
	if report.at.Before(bound.Add(-earliestReport)) {
		t.Fatalf("lost lock reported %s before %s", bound.Sub(report.at), bound)
	}
}

func TestKeepRenewedReportsAnOutageLongerThanTheDeclaredLease(t *testing.T) {
	gateway := newGateway(t)
	set := gateway.acquire(t)
	declared, err := hostlock.Extend(t.Context(), set.StateDir(), gatewayName, declaredLease)
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}
	gateway.goOffline(t)

	report := reportedLoss(t, set)

	bound := declared.Add(-shortLease().RenewInterval)
	if report.at.After(bound.Add(reportSlack)) {
		t.Fatalf("lost lock reported %s after %s, want a report one renewal interval before the declared expiry %s",
			report.at.Sub(bound), bound, declared)
	}
	if report.at.Before(bound.Add(-earliestReport)) {
		t.Fatalf("lost lock reported %s before %s, want the declared lease of %s in effect",
			bound.Sub(report.at), bound, declaredLease)
	}
}
