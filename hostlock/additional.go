package hostlock

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

// AcquireAdditional locks new hosts while the existing set continues renewal.
// A failed acquisition releases the new locks and retains the existing locks.
// Scope must run again after this method to include the additional hosts.
func (s *Set) AcquireAdditional(ctx context.Context, hosts []Host) error {
	s.membership.Lock()
	defer s.membership.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	pending, current, err := s.additionalHosts(hosts)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	var acquired []Host
	adopted := false
	defer func() {
		if !adopted {
			s.releaseAdditional(context.WithoutCancel(ctx), acquired)
		}
	}()
	deadlines := make(map[string]time.Time, len(pending))
	request := command{op: Acquire, run: s.run, controller: s.controller, lease: s.lease.TTL}
	for _, host := range pending {
		asked := clock.NowUTC()
		granted, err := runScript(ctx, host, request)
		if err != nil {
			slog.Error("hostlock.additional_acquire_failed", "host", host.Name, "run", s.run, "err", err)
			return fmt.Errorf("lock additional host %s: %w", host.Name, err)
		}
		acquired = append(acquired, host)
		deadlines[host.Name] = asked.Add(granted.remaining - s.lease.RenewInterval - hostClockStep)
	}
	if err := s.usable(); err != nil {
		return err
	}
	for name, deadline := range deadlines {
		if !clock.NowUTC().Before(deadline) {
			return fmt.Errorf("additional host %s passed its lock loss deadline during acquisition", name)
		}
	}
	if err := writeState(s.stateDir, s.run, s.controller, append(current, acquired...)); err != nil {
		return err
	}
	s.mu.Lock()
	s.held = append(s.held, acquired...)
	maps.Copy(s.deadlines, deadlines)
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
	adopted = true
	slog.Info("hostlock.additional_acquired", "run", s.run, "hosts", len(acquired))
	return nil
}

func (s *Set) additionalHosts(hosts []Host) ([]Host, []Host, error) {
	s.mu.Lock()
	current := slices.Clone(s.held)
	s.mu.Unlock()
	known := make(map[string]Host, len(current)+len(hosts))
	for _, host := range current {
		known[host.Name] = host
	}
	var pending []Host
	for _, host := range hosts {
		if previous, exists := known[host.Name]; exists {
			if previous != host {
				return nil, nil, fmt.Errorf("host %s differs from the endpoint already locked by this run", host.Name)
			}
			continue
		}
		known[host.Name] = host
		pending = append(pending, host)
	}
	return pending, current, nil
}

func (s *Set) releaseAdditional(ctx context.Context, hosts []Host) {
	for _, host := range hosts {
		if err := Do(ctx, host, Release, s.run, s.controller); err != nil {
			slog.Warn("hostlock.additional_release_failed", "host", host.Name, "run", s.run, "err", err)
		}
	}
}
