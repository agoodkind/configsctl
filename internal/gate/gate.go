package gate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/clock"
	"goodkind.io/configsctl/internal/hostlock"
	"goodkind.io/configsctl/internal/runid"
)

// requestCommand is the only SSH command a requester may send.
const requestCommand = "request"

// ControllerEnvVar defines the environment key for the controller name.
// configsctl writes the controller name into each host-lock record.
const ControllerEnvVar = "CONFIGS_CONTROLLER"

var requesterPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Server answers one request on a controller.
type Server struct {
	Repo       Repo
	Runs       Runs
	Requester  string
	Controller string
	// Self is the path of the configsctl binary that runs gate-finish.
	Self string
	Out  io.Writer
}

// Serve reads the request on in and writes the answer to s.Out.
func (s Server) Serve(ctx context.Context, sshCommand string, in io.Reader) error {
	if !requesterPattern.MatchString(s.Requester) {
		return fmt.Errorf("requester %q is not a requester name", s.Requester)
	}
	if strings.TrimSpace(sshCommand) != requestCommand {
		return fmt.Errorf("this key runs only %q with a JSON request on stdin", requestCommand)
	}
	req, err := DecodeRequest(in)
	if err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	slog.Info("gate.request.accepted", "requester", s.Requester, "kind", string(req.Kind), "session", req.Session)
	switch req.Kind {
	case KindDeploy, KindTofu:
		return s.start(ctx, req)
	case KindStatus:
		return s.status(ctx, req.Run)
	case KindLogs:
		return s.logs(ctx, req.Run, req.Follow)
	case KindUnlock:
		return s.unlock(ctx, req)
	case KindRuns:
		return s.list()
	default:
		return fmt.Errorf("unknown request kind %q", req.Kind)
	}
}

func (s Server) start(ctx context.Context, req Request) error {
	if err := s.Repo.Fetch(ctx); err != nil {
		return err
	}
	if err := s.Repo.RequireOnMain(ctx, req.Commit); err != nil {
		return err
	}
	run, err := runid.New()
	if err != nil {
		slog.Error("gate.run.id_failed", "err", err)
		return fmt.Errorf("create run id: %w", err)
	}
	if err := s.Runs.Create(run); err != nil {
		return err
	}
	rec := Record{
		Run: run, Controller: s.Controller, Requester: s.Requester, Request: req, Started: clock.Stamp(),
		Ended: "", ExitStatus: nil, ServiceResult: "", Error: "",
	}
	if err := s.Runs.Write(rec); err != nil {
		return err
	}
	worktree := s.Runs.worktree(run)
	if err := s.Repo.AddWorktree(ctx, worktree, req.Commit); err != nil {
		return s.fail(rec, err)
	}
	if req.Kind == KindDeploy {
		playbook := filepath.Join(worktree, "ansible", "playbooks", req.Playbook+".yml")
		if _, err := os.Stat(playbook); err != nil {
			return s.fail(rec, fmt.Errorf("playbook %s does not exist at commit %s", req.Playbook, req.Commit))
		}
	}
	if err := s.startUnit(ctx, run, worktree, runCommand(worktree, req)); err != nil {
		return s.fail(rec, err)
	}
	return writeJSON(s.Out, startAnswer{Run: run, Controller: s.Controller})
}

type startAnswer struct {
	Run        string `json:"run"`
	Controller string `json:"controller"`
}

type statusAnswer struct {
	Record Record `json:"record"`
	State  string `json:"state"`
}

type unlockAnswer struct {
	Unlocked string `json:"unlocked"`
	Run      string `json:"run"`
}

func runCommand(worktree string, req Request) []string {
	wrapper := filepath.Join(worktree, "configsctl")
	if req.Kind == KindDeploy {
		args := []string{wrapper, "deploy", req.Playbook}
		if req.Limit != "" {
			args = append(args, "--limit", req.Limit)
		}
		if len(req.ExtraVars) > 0 {
			args = append(args, "--extra-var", string(req.ExtraVars))
		}
		return args
	}
	args := []string{wrapper, "tofu", req.Workspace, string(req.Action), "-input=false"}
	if req.Action == TofuApply {
		args = append(args, "-auto-approve")
	}
	for _, target := range req.Targets {
		args = append(args, "-target="+target)
	}
	return args
}

func (s Server) startUnit(ctx context.Context, run, worktree string, command []string) error {
	args := []string{
		"--unit=" + unitPrefix + run,
		"--collect",
		"--service-type=exec",
		"--working-directory=" + worktree,
		"--setenv=HOME=/root",
		"--setenv=TMPDIR=" + s.Runs.tempDir(run),
		"--setenv=" + runid.EnvVar + "=" + run,
		"--setenv=" + ControllerEnvVar + "=" + s.Controller,
		"--property=ExecStopPost=" + s.Self + " " + finishCommand + " " + run,
		"--",
	}
	safe, err := checkedArgs(append(args, command...))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "systemd-run", safe...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		slog.Error("gate.run.start_failed", "run", run, "output", strings.TrimSpace(string(output)), "err", err)
		return fmt.Errorf("start run unit %s: %w: %s", run, err, strings.TrimSpace(string(output)))
	}
	slog.Info("gate.run.started", "run", run, "requester", s.Requester)
	return nil
}

func (s Server) fail(rec Record, cause error) error {
	rec.Ended = clock.Stamp()
	rec.Error = cause.Error()
	if err := s.Runs.Write(rec); err != nil {
		slog.Error("gate.run.failure_record_failed", "run", rec.Run, "err", err)
		return errors.Join(cause, err)
	}
	return cause
}

func (s Server) status(ctx context.Context, run string) error {
	rec, err := s.Runs.Read(run)
	if err != nil {
		return err
	}
	state := "finished"
	if rec.Ended == "" {
		state = unitState(ctx, run)
	}
	return writeJSON(s.Out, statusAnswer{Record: rec, State: state})
}

func unitState(ctx context.Context, run string) string {
	safe, err := checkedArgs([]string{"show", unitPrefix + run, "--property=ActiveState", "--value"})
	if err != nil {
		return "unknown"
	}
	out, err := exec.CommandContext(ctx, "systemctl", safe...).Output()
	if err != nil {
		slog.Warn("gate.run.state_unavailable", "run", run, "err", err)
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (s Server) unlock(ctx context.Context, req Request) error {
	inv, err := ansible.LoadInventory(ctx, s.Repo.Dir)
	if err != nil {
		slog.Error("gate.unlock.inventory_failed", "err", err)
		return fmt.Errorf("load the inventory for unlock: %w", err)
	}
	targets := hostlock.Targets(inv, []string{req.Host})
	if len(targets) == 0 {
		return fmt.Errorf("host %s has no ssh lock target", req.Host)
	}
	if err := hostlock.Do(ctx, targets[0], hostlock.Unlock, req.Run, s.Controller); err != nil {
		slog.Error("gate.unlock.failed", "host", req.Host, "run", req.Run, "err", err)
		return fmt.Errorf("unlock %s: %w", req.Host, err)
	}
	slog.Info("gate.unlock.done", "host", req.Host, "run", req.Run, "requester", s.Requester, "reason", req.Reason)
	return writeJSON(s.Out, unlockAnswer{Unlocked: req.Host, Run: req.Run})
}

type answer interface {
	startAnswer | statusAnswer | unlockAnswer | []Record
}

func (s Server) list() error {
	records, err := s.Runs.List()
	if err != nil {
		return err
	}
	return writeJSON(s.Out, records)
}

func writeJSON[T answer](w io.Writer, value T) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		slog.Error("gate.answer.write_failed", "err", err)
		return fmt.Errorf("write answer: %w", err)
	}
	return nil
}
