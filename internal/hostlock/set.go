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

// ErrLost indicates another owner or failed renewal after lostAfter.
// A renewal failure does not establish whether the remote lease has expired.
var ErrLost = errors.New("the run lost a host lock")

const lostAfter = TTL - RenewInterval

// Set is the locks of one run on its target hosts.
type Set struct {
	run        string
	controller string
	mu         sync.Mutex
	held       []Host
	renewed    map[string]time.Time
}

// AcquireAll requires a lock on every host. On failure, it attempts to release
// the locks already acquired and returns the error.
func AcquireAll(ctx context.Context, hosts []Host, run, controller string) (*Set, error) {
	set := &Set{run: run, controller: controller, mu: sync.Mutex{}, held: nil, renewed: map[string]time.Time{}}
	for _, host := range hosts {
		if err := Do(ctx, host, Acquire, run, controller); err != nil {
			set.ReleaseAll(context.WithoutCancel(ctx))
			slog.Error("hostlock.acquire_failed", "host", host.Name, "run", run, "err", err)
			return nil, fmt.Errorf("lock %s: %w", host.Name, err)
		}
		set.held = append(set.held, host)
		set.renewed[host.Name] = clock.NowUTC()
	}
	slog.Info("hostlock.acquired", "run", run, "hosts", len(set.held))
	return set, nil
}

// RenewAll renews every lock of the set. It returns an error that wraps
// ErrLost when another run owns a lock, or when a lock has not been renewed
// for lostAfter.
func (s *Set) RenewAll(ctx context.Context) error {
	s.mu.Lock()
	hosts := append([]Host{}, s.held...)
	s.mu.Unlock()
	for _, host := range hosts {
		err := Do(ctx, host, Renew, s.run, s.controller)
		s.mu.Lock()
		if err == nil {
			s.renewed[host.Name] = clock.NowUTC()
		}
		since := clock.NowUTC().Sub(s.renewed[host.Name])
		s.mu.Unlock()
		switch {
		case errors.Is(err, ErrNotHeld):
			slog.Error("hostlock.lost", "host", host.Name, "run", s.run, "err", err)
			return fmt.Errorf("%w: %s: %w", ErrLost, host.Name, err)
		case err != nil && since >= lostAfter:
			slog.Error("hostlock.lost", "host", host.Name, "run", s.run, "since_renewal", since.String(), "err", err)
			return fmt.Errorf("%w: %s was not renewed for %s: %w", ErrLost, host.Name, since.Round(time.Second), err)
		case err != nil:
			slog.Warn("hostlock.renew_failed", "host", host.Name, "run", s.run, "err", err)
		}
	}
	return nil
}

// KeepRenewed runs RenewAll each RenewInterval until the returned stop
// function runs. When RenewAll returns ErrLost, KeepRenewed calls lost once
// and stops renewing.
func (s *Set) KeepRenewed(ctx context.Context, lost func(error)) (stop func()) {
	renewCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("hostlock.renew_panicked", "run", s.run, "err", fmt.Errorf("%v", r))
			}
		}()
		ticker := time.NewTicker(RenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := s.RenewAll(renewCtx); errors.Is(err, ErrLost) {
					lost(err)
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
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
}
