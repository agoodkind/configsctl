package hostlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

// ErrLost reports a lock owned by another run or a host that has not
// answered before its loss deadline.
// The loss deadline is one RenewInterval before the reported lock expiry.
var ErrLost = errors.New("the run lost a host lock")

// Lease defines the lock lifetime and the interval between renewals.
type Lease struct {
	TTL           time.Duration
	RenewInterval time.Duration
}

// DefaultLease returns a 300-second lifetime and a 60-second renewal
// interval.
func DefaultLease() Lease {
	return Lease{TTL: TTL, RenewInterval: RenewInterval}
}

// Set is the locks of one run on its target hosts.
type Set struct {
	run        string
	controller string
	lease      Lease
	stateDir   string
	mu         sync.Mutex
	held       []Host
	deadlines  map[string]time.Time
}

// AcquireAll requires a lock on every host. On failure, it attempts to release
// the locks already acquired and returns the error.
func AcquireAll(ctx context.Context, hosts []Host, run, controller string, lease Lease) (*Set, error) {
	if lease.TTL < time.Second || lease.RenewInterval <= 0 || lease.RenewInterval >= lease.TTL {
		return nil, fmt.Errorf("lease of %s with renewal each %s is not usable", lease.TTL, lease.RenewInterval)
	}
	stateDir, err := newStateDir()
	if err != nil {
		return nil, err
	}
	set := &Set{
		run: run, controller: controller, lease: lease, stateDir: stateDir,
		mu: sync.Mutex{}, held: nil, deadlines: map[string]time.Time{},
	}
	request := command{op: Acquire, run: run, controller: controller, lease: lease.TTL}
	for _, host := range hosts {
		asked := clock.NowUTC()
		granted, err := runScript(ctx, host, request)
		if err != nil {
			set.ReleaseAll(context.WithoutCancel(ctx))
			slog.Error("hostlock.acquire_failed", "host", host.Name, "run", run, "err", err)
			return nil, fmt.Errorf("lock %s: %w", host.Name, err)
		}
		set.held = append(set.held, host)
		set.advance(host.Name, asked, granted.remaining)
	}
	if err := writeState(stateDir, run, controller, set.held); err != nil {
		set.ReleaseAll(context.WithoutCancel(ctx))
		return nil, err
	}
	slog.Info("hostlock.acquired", "run", run, "hosts", len(set.held))
	return set, nil
}

// StateDir returns the directory that Extend reads and writes for this set.
func (s *Set) StateDir() string {
	return s.stateDir
}

// hostClockStep is the unit of the host time that the lock script reads.
const hostClockStep = time.Second

// advance sets the host's loss deadline one RenewInterval before the expiry
// reported for the script call sent at asked.
// The lock script rounds host time down by up to hostClockStep.
// advance never sets an earlier deadline.
func (s *Set) advance(name string, asked time.Time, remaining time.Duration) {
	deadline := asked.Add(remaining - s.lease.RenewInterval - hostClockStep)
	s.mu.Lock()
	defer s.mu.Unlock()
	if deadline.After(s.deadlines[name]) {
		s.deadlines[name] = deadline
	}
}

// overdue returns the first locked host with a deadline at or before now.
// When no host is overdue, overdue returns the earliest deadline in the set.
func (s *Set) overdue(now time.Time) (name string, deadline time.Time, lost bool) {
	s.readDeclarations()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, host := range s.held {
		hostDeadline := s.deadlines[host.Name]
		if !now.Before(hostDeadline) {
			return host.Name, hostDeadline, true
		}
		if deadline.IsZero() || hostDeadline.Before(deadline) {
			name, deadline = host.Name, hostDeadline
		}
	}
	return name, deadline, false
}

func (s *Set) deadline(name string) time.Time {
	s.readDeclarations()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deadlines[name]
}

// RenewAll returns an error wrapping ErrLost when another run owns a lock
// or a host has not answered before its loss deadline.
func (s *Set) RenewAll(ctx context.Context) error {
	s.mu.Lock()
	hosts := append([]Host{}, s.held...)
	s.mu.Unlock()
	request := command{op: Renew, run: s.run, controller: s.controller, lease: s.lease.TTL}
	for _, host := range hosts {
		asked := clock.NowUTC()
		renewed, err := runScript(ctx, host, request)
		if err == nil {
			s.advance(host.Name, asked, renewed.remaining)
			continue
		}
		if errors.Is(err, ErrNotHeld) {
			slog.Error("hostlock.lost", "host", host.Name, "run", s.run, "err", err)
			return fmt.Errorf("%w: %s: %w", ErrLost, host.Name, err)
		}
		deadline := s.deadline(host.Name)
		if !clock.NowUTC().Before(deadline) {
			slog.Error("hostlock.lost", "host", host.Name, "run", s.run, "deadline", deadline, "err", err)
			return fmt.Errorf("%w: %s gave no answer until %s: %w",
				ErrLost, host.Name, deadline.Format(time.RFC3339), err)
		}
		slog.Warn("hostlock.renew_failed", "host", host.Name, "run", s.run, "deadline", deadline, "err", err)
	}
	return nil
}

// KeepRenewed runs RenewAll each RenewInterval until the returned stop
// function runs.
// KeepRenewed calls lost once with an error wrapping ErrLost and stops
// renewing when RenewAll returns ErrLost or a host's loss deadline passes.
func (s *Set) KeepRenewed(ctx context.Context, lost func(error)) (stop func()) {
	renewCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	report := func(err error) {
		if renewCtx.Err() == nil {
			once.Do(func() { lost(err) })
		}
		cancel()
	}
	var running sync.WaitGroup
	for _, loop := range []func(context.Context, func(error)){s.renewEachInterval, s.watchDeadlines} {
		running.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("hostlock.renew_panicked", "run", s.run, "err", fmt.Errorf("%v", r))
				}
			}()
			loop(renewCtx, report)
		})
	}
	return func() {
		cancel()
		running.Wait()
	}
}

func (s *Set) renewEachInterval(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(s.lease.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RenewAll(ctx); errors.Is(err, ErrLost) {
				report(err)
				return
			}
		}
	}
}

// watchDeadlines reports a loss at the loss deadline of a host. A renewal
// that blocks, and the time between two renewals, do not delay the report.
func (s *Set) watchDeadlines(ctx context.Context, report func(error)) {
	for {
		now := clock.NowUTC()
		name, deadline, lost := s.overdue(now)
		if lost {
			err := fmt.Errorf("%w: %s gave no answer until %s", ErrLost, name, deadline.Format(time.RFC3339))
			slog.Error("hostlock.lost", "host", name, "run", s.run, "deadline", deadline, "err", err)
			report(err)
			return
		}
		if deadline.IsZero() {
			return
		}
		timer := time.NewTimer(deadline.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// ReleaseAll deletes every lock file that this run took.
func (s *Set) ReleaseAll(ctx context.Context) {
	s.mu.Lock()
	hosts := s.held
	s.held = nil
	s.mu.Unlock()
	for _, host := range hosts {
		if err := Do(ctx, host, Release, s.run, s.controller); err != nil {
			slog.Warn("hostlock.release_failed", "host", host.Name, "run", s.run, "err", err)
		}
	}
	removeStateDir(s.stateDir)
}
