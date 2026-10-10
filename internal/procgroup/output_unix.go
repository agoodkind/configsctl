//go:build unix

package procgroup

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"time"
)

var errOutputOpen = errors.New("the command output stayed open after its process group stopped")

type outputCopier struct {
	readEnds  []*os.File
	writeEnds []*os.File
	copies    sync.WaitGroup
	mu        sync.Mutex
	copyErr   error
}

func pipeOutput(cmd *exec.Cmd) (*outputCopier, error) {
	copier := &outputCopier{
		readEnds:  nil,
		writeEnds: nil,
		copies:    sync.WaitGroup{},
		mu:        sync.Mutex{},
		copyErr:   nil,
	}
	sharedStderr := sameWriter(cmd.Stdout, cmd.Stderr)
	stdout, err := copier.attach(cmd.Stdout)
	if err != nil {
		copier.abort()
		return nil, err
	}
	cmd.Stdout = stdout
	if sharedStderr {
		cmd.Stderr = stdout
		return copier, nil
	}
	stderr, err := copier.attach(cmd.Stderr)
	if err != nil {
		copier.abort()
		return nil, err
	}
	cmd.Stderr = stderr
	return copier, nil
}

func sameWriter(first io.Writer, second io.Writer) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	firstType := reflect.TypeOf(first)
	if firstType != reflect.TypeOf(second) {
		return false
	}
	kind := firstType.Kind()
	if kind != reflect.Pointer && kind != reflect.Chan && kind != reflect.UnsafePointer {
		return false
	}
	return first == second
}

func (c *outputCopier) attach(destination io.Writer) (io.Writer, error) {
	if _, isFile := destination.(*os.File); isFile || destination == nil {
		return destination, nil
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		slog.Error("procgroup.output_pipe_failed", "err", err)
		return nil, fmt.Errorf("create output pipe: %w", err)
	}
	c.readEnds = append(c.readEnds, readEnd)
	c.writeEnds = append(c.writeEnds, writeEnd)
	c.copies.Add(1)
	goRecovering("procgroup.output_copy_panicked", func() { c.copy(destination, readEnd) })
	return writeEnd, nil
}

func goRecovering(event string, run func()) {
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error(event, "err", fmt.Errorf("%v", recovered))
			}
		}()
		run()
	}()
}

func (c *outputCopier) copy(destination io.Writer, source *os.File) {
	defer c.copies.Done()
	_, err := io.Copy(destination, source)
	if err == nil || errors.Is(err, os.ErrClosed) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.copyErr == nil {
		c.copyErr = err
	}
}

func (c *outputCopier) started() {
	for _, writeEnd := range c.writeEnds {
		_ = writeEnd.Close()
	}
}

func (c *outputCopier) abort() {
	c.started()
	c.closeReadEnds()
	c.copies.Wait()
}

func (c *outputCopier) closeReadEnds() {
	for _, readEnd := range c.readEnds {
		_ = readEnd.Close()
	}
}

func (c *outputCopier) wait() error {
	done := make(chan struct{})
	goRecovering("procgroup.output_wait_panicked", func() {
		c.copies.Wait()
		close(done)
	})
	grace := time.NewTimer(StopGrace)
	defer grace.Stop()
	select {
	case <-done:
	case <-grace.C:
		slog.Error("procgroup.output_copy_abandoned", "err", errOutputOpen)
		c.closeReadEnds()
		<-done
	}
	c.closeReadEnds()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.copyErr != nil {
		slog.Error("procgroup.output_copy_failed", "err", c.copyErr)
		return fmt.Errorf("copy command output: %w", c.copyErr)
	}
	return nil
}
