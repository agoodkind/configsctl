package hostlock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Set is the locks of one run on its target hosts.
type Set struct {
	run        string
	controller string
	mu         sync.Mutex
	held       []Host
}

// AcquireAll releases acquired locks and returns the error when Do returns HeldError.
// AcquireAll logs other Do errors as hostlock.skipped and skips the affected hosts.
func AcquireAll(ctx context.Context, hosts []Host, run, controller string) (*Set, error) {
	set := &Set{run: run, controller: controller, mu: sync.Mutex{}, held: nil}
	for _, host := range hosts {
		err := Do(ctx, host, Acquire, run, controller)
		var held *HeldError
		switch {
		case err == nil:
			set.held = append(set.held, host)
		case errors.As(err, &held):
			set.ReleaseAll(context.WithoutCancel(ctx))
			return nil, err
		default:
			slog.Warn("hostlock.skipped", "host", host.Name, "run", run, "err", err)
		}
	}
	slog.Info("hostlock.acquired", "run", run, "hosts", len(set.held), "requested", len(hosts))
	return set, nil
}

// KeepRenewed renews every taken lock each RenewInterval until the returned
// stop function runs.
func (s *Set) KeepRenewed(ctx context.Context) (stop func()) {
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
				s.renewOnce(renewCtx)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func (s *Set) renewOnce(ctx context.Context) {
	s.mu.Lock()
	hosts := append([]Host{}, s.held...)
	s.mu.Unlock()
	for _, host := range hosts {
		if err := Do(ctx, host, Renew, s.run, s.controller); err != nil {
			slog.Warn("hostlock.renew_failed", "host", host.Name, "run", s.run, "err", err)
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
}
