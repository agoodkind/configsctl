package hostlock

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

// ErrScope reports a scope that does not permit an OpenTofu mutation.
var ErrScope = errors.New("the host lock scope does not permit mutation")

// Scope permits OpenTofu mutation while its Set retains the acquired host locks.
// Only Set.Scope creates a Scope. A Scope is valid only in the process that
// created it. StateEnvVar does not create or replace a Scope.
type Scope struct {
	process    int
	run        string
	controller string
	set        *Set
	hosts      []string
}

// Scope returns a Scope for the hosts that s locked. It returns an error
// wrapping ErrScope after s releases its locks, loses a lock, ends its
// renewal, or passes a loss deadline. A Set without hosts returns a valid
// Scope.
func (s *Set) Scope() (*Scope, error) {
	if err := s.usable(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	hosts := make([]string, 0, len(s.held))
	for _, host := range s.held {
		hosts = append(hosts, host.Name)
	}
	s.mu.Unlock()
	return &Scope{process: os.Getpid(), run: s.run, controller: s.controller, set: s, hosts: hosts}, nil
}

// Check returns nil when scope permits mutation on every host in
// requiredHosts. Check returns an error wrapping ErrScope for a nil scope, a
// scope from another process, a Set that Scope would refuse, or a required
// host without a lock in the scope.
func (s *Scope) Check(requiredHosts []string) error {
	if s == nil {
		return scopeRefusal("", "the request does not supply a host lock scope")
	}
	if process := os.Getpid(); s.process != process {
		return scopeRefusal(s.run, fmt.Sprintf("process %d created the scope, not process %d", s.process, process))
	}
	if err := s.set.usable(); err != nil {
		return err
	}
	for _, name := range requiredHosts {
		if !slices.Contains(s.hosts, name) {
			return scopeRefusal(s.run, fmt.Sprintf("controller %s did not lock host %s", s.controller, name))
		}
	}
	return nil
}

// usable returns an error wrapping ErrScope when s released its locks, lost
// a lock, ended its renewal, or passed a loss deadline.
func (s *Set) usable() error {
	s.mu.Lock()
	released, lost, renewalDone := s.released, s.lost, s.renewalDone
	s.mu.Unlock()
	switch {
	case released:
		return scopeRefusal(s.run, "the run released its host locks")
	case lost != nil:
		return scopeRefusal(s.run, lost.Error())
	case renewalEnded(renewalDone):
		return scopeRefusal(s.run, "the lock renewal ended")
	}
	if name, deadline, expired := s.overdue(clock.NowUTC()); expired {
		reason := fmt.Sprintf("the lock on host %s passed its loss deadline %s", name, deadline.Format(time.RFC3339))
		return scopeRefusal(s.run, reason)
	}
	return nil
}

// renewalEnded reports whether KeepRenewed started and then stopped.
func renewalEnded(done <-chan struct{}) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

func scopeRefusal(run, reason string) error {
	err := fmt.Errorf("%w: %s", ErrScope, reason)
	slog.Error("hostlock.scope_refused", "run", run, "reason", reason, "err", err)
	return err
}
