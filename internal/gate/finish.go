package gate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"goodkind.io/configsctl/internal/clock"
)

const followPoll = time.Second

// Finish records the end of run. systemd runs it as ExecStopPost, with
// EXIT_STATUS and SERVICE_RESULT in the environment. It deletes the run
// worktree and keeps the run log.
func (r Runs) Finish(ctx context.Context, repo Repo, run string) error {
	if !runPattern.MatchString(run) {
		return fmt.Errorf("run %q is not a run id", run)
	}
	rec, err := r.Read(run)
	if err != nil {
		return err
	}
	rec.Ended = clock.Stamp()
	rec.ServiceResult = os.Getenv("SERVICE_RESULT")
	if status, err := strconv.Atoi(os.Getenv("EXIT_STATUS")); err == nil {
		rec.ExitStatus = &status
	}
	if err := r.Write(rec); err != nil {
		return err
	}
	if err := repo.RemoveWorktree(ctx, r.worktree(run)); err != nil {
		slog.Warn("gate.run.worktree_remove_failed", "run", run, "err", err)
	}
	slog.Info("gate.run.finished", "run", run, "requester", rec.Requester, "result", rec.ServiceResult)
	return nil
}

func (s Server) logs(ctx context.Context, run string, follow bool) error {
	if _, err := s.Runs.Read(run); err != nil {
		return err
	}
	journalArgs, err := checkedArgs([]string{"--unit=" + unitPrefix + run, "--output=cat", "--no-pager"})
	if err != nil {
		return err
	}
	journal := exec.CommandContext(ctx, "journalctl", journalArgs...)
	journal.Stdout = s.Out
	journal.Stderr = s.Out
	if err := journal.Run(); err != nil {
		slog.Warn("gate.logs.journal_failed", "run", run, "err", err)
	}
	var offset int64
	for {
		written, err := s.copyLogFrom(run, offset)
		if err != nil {
			return err
		}
		offset += written
		if !follow {
			return nil
		}
		rec, err := s.Runs.Read(run)
		if err != nil {
			return err
		}
		if rec.Ended != "" && written == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(followPoll):
		}
	}
}

func (s Server) copyLogFrom(run string, offset int64) (int64, error) {
	path := s.Runs.PlayLog(run)
	if path == "" {
		return 0, nil
	}
	file, err := os.Open(path)
	if err != nil {
		slog.Error("gate.logs.open_failed", "run", run, "err", err)
		return 0, fmt.Errorf("open run log of %s: %w", run, err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		slog.Error("gate.logs.seek_failed", "run", run, "err", err)
		return 0, fmt.Errorf("seek run log of %s: %w", run, err)
	}
	written, err := io.Copy(s.Out, file)
	if err != nil {
		slog.Error("gate.logs.copy_failed", "run", run, "err", err)
		return written, fmt.Errorf("copy run log of %s: %w", run, err)
	}
	return written, nil
}
