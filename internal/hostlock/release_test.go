package hostlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/configsctl/internal/hostlock"
)

const (
	testRetryInterval = 20 * time.Millisecond
	repairDelay       = 150 * time.Millisecond
	shortBound        = 300 * time.Millisecond
	longBound         = 30 * time.Second
	hungReleaseLimit  = 5 * time.Second
)

type switchedHost struct {
	host     hostlock.Host
	lockFile string
	link     string
	realDir  string
}

func lockedSwitchedHost(t *testing.T) (switchedHost, *hostlock.Set) {
	t.Helper()
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatalf("create the lock directory: %v", err)
	}
	link := filepath.Join(base, "locks")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("link the lock directory: %v", err)
	}
	host := hostlock.Host{Name: "switched", User: "", Address: "", Dir: link}
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, firstRun, "controller-a")
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	switched := switchedHost{host: host, lockFile: filepath.Join(realDir, "deploy.lock"), link: link, realDir: realDir}
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove the lock directory link: %v", err)
	}
	if err := os.WriteFile(link, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}
	return switched, set
}

func (s switchedHost) repair() error {
	replacement := s.link + ".new"
	if err := os.Symlink(s.realDir, replacement); err != nil {
		return err
	}
	return os.Rename(replacement, s.link)
}

func TestReleaseAllRetriesUntilTheReleaseSucceeds(t *testing.T) {
	switched, set := lockedSwitchedHost(t)
	set.ReleaseRetryInterval = testRetryInterval
	set.ReleaseRetryBound = longBound
	repaired := make(chan error, 1)
	go func() {
		time.Sleep(repairDelay)
		repaired <- switched.repair()
	}()
	set.ReleaseAll(t.Context())
	if err := <-repaired; err != nil {
		t.Fatalf("repair the lock directory: %v", err)
	}
	if _, err := os.Stat(switched.lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock file remains after ReleaseAll: %v", err)
	}
}

func TestReleaseAllStopsAtTheBoundWhenTheReleaseKeepsFailing(t *testing.T) {
	switched, set := lockedSwitchedHost(t)
	set.ReleaseRetryInterval = testRetryInterval
	set.ReleaseRetryBound = shortBound
	before, err := os.ReadFile(switched.lockFile)
	if err != nil {
		t.Fatalf("read the lock file: %v", err)
	}
	started := time.Now()
	set.ReleaseAll(t.Context())
	elapsed := time.Since(started)
	if elapsed < shortBound || elapsed > longBound {
		t.Fatalf("ReleaseAll returned after %s, want at least %s and under %s", elapsed, shortBound, longBound)
	}
	after, err := os.ReadFile(switched.lockFile)
	if err != nil || string(after) != string(before) {
		t.Fatalf("lock file = %q, err = %v; want %q", after, err, before)
	}
}

func TestReleaseAllReturnsAtTheBoundWhenAReleaseCommandHangs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root creates the guard directory in a read-only lock directory")
	}
	host := localHost(t)
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, firstRun, "controller-a")
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	if err := os.Chmod(host.Dir, 0o500); err != nil {
		t.Fatalf("make the lock directory read-only: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(host.Dir, 0o700); err != nil {
			t.Errorf("make the lock directory writable: %v", err)
		}
	})
	set.ReleaseRetryInterval = testRetryInterval
	set.ReleaseRetryBound = shortBound
	started := time.Now()
	set.ReleaseAll(t.Context())
	elapsed := time.Since(started)
	if elapsed < shortBound || elapsed > hungReleaseLimit {
		t.Fatalf("ReleaseAll returned after %s, want at least %s and under %s", elapsed, shortBound, hungReleaseLimit)
	}
	if _, err := os.Stat(filepath.Join(host.Dir, "deploy.lock")); err != nil {
		t.Fatalf("the lock file after the hung release: %v", err)
	}
}

func TestReleaseAllStopsWhenTheContextIsDone(t *testing.T) {
	switched, set := lockedSwitchedHost(t)
	set.ReleaseRetryInterval = testRetryInterval
	set.ReleaseRetryBound = longBound
	ctx, cancel := context.WithTimeout(t.Context(), shortBound)
	defer cancel()
	started := time.Now()
	set.ReleaseAll(ctx)
	if elapsed := time.Since(started); elapsed > longBound/2 {
		t.Fatalf("ReleaseAll returned after %s, want it to stop with the context after %s", elapsed, shortBound)
	}
	if _, err := os.Stat(switched.lockFile); err != nil {
		t.Fatalf("the lock file after the stopped release: %v", err)
	}
}

func TestReleaseAllReturnsWithoutDelayWhenTheReleaseSucceeds(t *testing.T) {
	host := localHost(t)
	set, err := hostlock.AcquireAll(t.Context(), []hostlock.Host{host}, firstRun, "controller-a")
	if err != nil {
		t.Fatalf("AcquireAll: %v", err)
	}
	started := time.Now()
	set.ReleaseAll(t.Context())
	if elapsed := time.Since(started); elapsed > hostlock.ReleaseRetryInterval/2 {
		t.Fatalf("ReleaseAll took %s on a healthy host, want under %s", elapsed, hostlock.ReleaseRetryInterval/2)
	}
	if _, err := os.Stat(filepath.Join(host.Dir, "deploy.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock file remains after ReleaseAll: %v", err)
	}
}
