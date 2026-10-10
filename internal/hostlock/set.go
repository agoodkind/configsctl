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

// ReleaseRetryInterval is the default delay between release retry rounds.
const ReleaseRetryInterval = 5 * time.Second

// ReleaseRetryBound limits release retries to two minutes.
// The bound is shorter than the five-minute lock expiry.
const ReleaseRetryBound = 2 * time.Minute

// Set is the locks of one run on its target hosts.
type Set struct {
	ReleaseRetryInterval time.Duration
	ReleaseRetryBound    time.Duration

	run        string
	controller string
	mu         sync.Mutex
	held       []Host
	renewed    map[string]time.Time
}

// AcquireAll requires a lock on every host. On failure, it attempts to release
// the locks already acquired and returns the error.
func AcquireAll(ctx context.Context, hosts []Host, run, controller string) (*Set, error) {
	set := &Set{
		ReleaseRetryInterval: ReleaseRetryInterval,
		ReleaseRetryBound:    ReleaseRetryBound,
		run:                  run,
		controller:           controller,
		mu:                   sync.Mutex{},
		held:                 nil,
		renewed:              map[string]time.Time{},
	}
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

// ReleaseAll attempts release on every locked host.
// ReleaseAll retries failed release commands every five seconds.
// Retries stop after two minutes or when the context is done.
// A first round without failures returns without waiting.
func (s *Set) ReleaseAll(ctx context.Context) {
	s.mu.Lock()
	hosts := s.held
	s.held = nil
	s.mu.Unlock()
	boundCtx, cancel := context.WithTimeout(ctx, s.ReleaseRetryBound)
	defer cancel()
	for attempt := 1; ; attempt++ {
		failed := s.releaseEach(boundCtx, hosts)
		if len(failed) == 0 {
			return
		}
		if !waitForReleaseRetry(boundCtx, s.ReleaseRetryInterval) {
			for _, failure := range failed {
				slog.Error("hostlock.release_failed", "host", failure.host.Name, "run", s.run,
					"attempts", attempt, "stderr", failure.stderr, "err", failure.err)
			}
			return
		}
		hosts = hosts[:0]
		for _, failure := range failed {
			hosts = append(hosts, failure.host)
		}
	}
}

type failedRelease struct {
	host   Host
	stderr string
	err    error
}

func (s *Set) releaseEach(ctx context.Context, hosts []Host) []failedRelease {
	var failed []failedRelease
	for _, host := range hosts {
		result := runScript(ctx, host, Release, lockOwner{run: s.run, controller: s.controller})
		if result.err == nil {
			continue
		}
		slog.Warn("hostlock.operation_failed", "host", host.Name, "operation", string(Release),
			"stderr", result.stderr, "err", result.err)
		failed = append(failed, failedRelease{host: host, stderr: result.stderr, err: result.err})
	}
	return failed
}

func waitForReleaseRetry(ctx context.Context, interval time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	retry := time.NewTimer(interval)
	defer retry.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-retry.C:
		return true
	}
}
