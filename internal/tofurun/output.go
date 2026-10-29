package tofurun

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"sync"

	"goodkind.io/configsctl/internal/redact"
)

// installedMu protects installedPatterns. InstallRedaction records its patterns
// in installedPatterns until its restore function removes them. Run uses these
// patterns to redact its run log.
var (
	installedMu       sync.Mutex
	installedPatterns []redact.Pattern
)

// lockedWriter serializes writes to a shared descriptor with a mutex. The
// stdout and stderr redactors cannot interleave partial writes.
type lockedWriter struct {
	mu  *sync.Mutex
	dst *os.File
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.dst.Write(p)
	if err != nil {
		return n, fmt.Errorf("locked write: %w", err)
	}
	return n, nil
}

// InstallRedaction filters [os.Stdout] and [os.Stderr] with a shared mutex and
// records patterns for Run logs until restoration. Empty patterns leave the
// descriptors unchanged. It returns a restore function or a pipe creation
// error. A pipe creation error prevents command execution.
func InstallRedaction(patterns []redact.Pattern) (func(), error) {
	realStdout, realStderr := os.Stdout, os.Stderr
	if len(patterns) == 0 {
		return func() {}, nil
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		slog.Error("stdout pipe failed", "err", err)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		slog.Error("stderr pipe failed", "err", err)
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	drain := func(src *os.File, dst *os.File) {
		defer wg.Done()
		w := redact.New(&lockedWriter{mu: &mu, dst: dst}, patterns)
		_, _ = io.Copy(w, src)
		_ = w.Close()
	}
	launch := func(src *os.File, dst *os.File) {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("redaction drain panicked", "err", fmt.Errorf("%v", r))
				}
			}()
			drain(src, dst)
		}()
	}
	wg.Add(2)
	launch(outR, realStdout)
	launch(errR, realStderr)

	os.Stdout = outW
	os.Stderr = errW
	registered := registerPatterns(patterns)

	restore := func() {
		registered()
		os.Stdout = realStdout
		os.Stderr = realStderr
		_ = outW.Close()
		_ = errW.Close()
		wg.Wait()
		_ = outR.Close()
		_ = errR.Close()
	}
	return restore, nil
}

// registerPatterns appends patterns to the installed set and returns a
// function that removes them. Callers remove patterns in reverse installation
// order.
func registerPatterns(patterns []redact.Pattern) func() {
	installedMu.Lock()
	defer installedMu.Unlock()
	start := len(installedPatterns)
	installedPatterns = append(installedPatterns, patterns...)
	return func() {
		installedMu.Lock()
		defer installedMu.Unlock()
		installedPatterns = installedPatterns[:start]
	}
}

// activePatterns returns a copy of the installed patterns.
func activePatterns() []redact.Pattern {
	installedMu.Lock()
	defer installedMu.Unlock()
	return slices.Clone(installedPatterns)
}
