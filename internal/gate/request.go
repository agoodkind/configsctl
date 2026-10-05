// Package gate reads JSON requests from stdin for configsctl gate.
// Server executes configsctl with an argument list.
// Server does not execute request fields as shell code.
package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
)

// Kind is the operation a request asks for.
type Kind string

// Validate refuses every kind outside this list.
const (
	KindDeploy Kind = "deploy"
	KindTofu   Kind = "tofu"
	KindStatus Kind = "status"
	KindLogs   Kind = "logs"
	KindUnlock Kind = "unlock"
	KindRuns   Kind = "runs"
)

// TofuAction is the OpenTofu operation of a tofu request.
type TofuAction string

// Validate refuses every action outside plan and apply.
const (
	TofuPlan  TofuAction = "plan"
	TofuApply TofuAction = "apply"
)

const maxRequestBytes = 64 << 10

// Request is the JSON object a client sends. Each kind uses a subset of the
// fields, and validation refuses a field that the kind does not use.
type Request struct {
	Kind      Kind            `json:"kind"`
	Playbook  string          `json:"playbook,omitempty"`
	Commit    string          `json:"commit,omitempty"`
	Limit     string          `json:"limit,omitempty"`
	ExtraVars json.RawMessage `json:"extra_vars,omitempty"`
	Session   string          `json:"session,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
	Action    TofuAction      `json:"action,omitempty"`
	Targets   []string        `json:"targets,omitempty"`
	Run       string          `json:"run,omitempty"`
	Follow    bool            `json:"follow,omitempty"`
	Host      string          `json:"host,omitempty"`
	Reason    string          `json:"reason,omitempty"`
}

var (
	playbookPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	commitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	limitPattern     = regexp.MustCompile(`^[A-Za-z0-9_.:,!&*-]{1,512}$`)
	sessionPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	workspacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	targetPattern    = regexp.MustCompile(`^[A-Za-z0-9_.\[\]"-]{1,256}$`)
	runPattern       = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)
	hostPattern      = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	reasonPattern    = regexp.MustCompile(`^[\x20-\x7E]{1,500}$`)
)

// DecodeRequest reads one request from r. It refuses an unknown field, a
// second JSON value, and a body larger than maxRequestBytes.
func DecodeRequest(r io.Reader) (Request, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxRequestBytes+1))
	if err != nil {
		slog.Error("gate.request.read_failed", "err", err)
		return Request{}, fmt.Errorf("read request: %w", err)
	}
	if len(body) > maxRequestBytes {
		return Request{}, fmt.Errorf("request is larger than %d bytes", maxRequestBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		slog.Error("gate.request.decode_failed", "err", err)
		return Request{}, fmt.Errorf("decode request: %w", err)
	}
	if decoder.More() {
		return Request{}, errors.New("request has more than one JSON value")
	}
	return req, nil
}

// Validate checks every field of the request against its kind. It does not
// check the repository or the hosts; Gate does that before it starts a run.
func (r Request) Validate() error {
	switch r.Kind {
	case KindDeploy:
		return r.validateDeploy()
	case KindTofu:
		return r.validateTofu()
	case KindStatus, KindLogs:
		return r.validateRunLookup()
	case KindUnlock:
		return r.validateUnlock()
	case KindRuns:
		return r.requireOnly()
	default:
		return fmt.Errorf("unknown request kind %q", r.Kind)
	}
}

func (r Request) validateDeploy() error {
	if err := r.requireOnly("playbook", "commit", "limit", "extra_vars", "session"); err != nil {
		return err
	}
	if !playbookPattern.MatchString(r.Playbook) {
		return fmt.Errorf("playbook %q is not a playbook name", r.Playbook)
	}
	if err := checkCommitAndSession(r.Commit, r.Session); err != nil {
		return err
	}
	if r.Limit != "" && !limitPattern.MatchString(r.Limit) {
		return fmt.Errorf("limit %q has a character outside the host pattern set", r.Limit)
	}
	if len(r.ExtraVars) > 0 && !isJSONObject(r.ExtraVars) {
		return errors.New("extra_vars is not a JSON object")
	}
	return nil
}

func (r Request) validateTofu() error {
	if err := r.requireOnly("workspace", "action", "commit", "targets", "session"); err != nil {
		return err
	}
	if !workspacePattern.MatchString(r.Workspace) {
		return fmt.Errorf("workspace %q is not a workspace name", r.Workspace)
	}
	if r.Action != TofuPlan && r.Action != TofuApply {
		return fmt.Errorf("action %q is not plan or apply", r.Action)
	}
	if err := checkCommitAndSession(r.Commit, r.Session); err != nil {
		return err
	}
	for _, target := range r.Targets {
		if !targetPattern.MatchString(target) {
			return fmt.Errorf("target %q is not a resource address", target)
		}
	}
	return nil
}

func (r Request) validateRunLookup() error {
	if err := r.requireOnly("run", "follow"); err != nil {
		return err
	}
	if !runPattern.MatchString(r.Run) {
		return fmt.Errorf("run %q is not a run id", r.Run)
	}
	if r.Kind == KindStatus && r.Follow {
		return errors.New("a status request does not use follow")
	}
	return nil
}

func (r Request) validateUnlock() error {
	if err := r.requireOnly("host", "run", "reason"); err != nil {
		return err
	}
	if !hostPattern.MatchString(r.Host) {
		return fmt.Errorf("host %q is not a host name", r.Host)
	}
	if !runPattern.MatchString(r.Run) {
		return fmt.Errorf("run %q is not a run id", r.Run)
	}
	if !reasonPattern.MatchString(r.Reason) {
		return errors.New("reason is empty, longer than 500 characters, or not printable ASCII")
	}
	return nil
}

func checkCommitAndSession(commit, session string) error {
	if !commitPattern.MatchString(commit) {
		return fmt.Errorf("commit %q is not a 40-character commit", commit)
	}
	if !sessionPattern.MatchString(session) {
		return fmt.Errorf("session %q is empty or has a character outside the session set", session)
	}
	return nil
}

// requireOnly refuses every set field that is not in allowed.
func (r Request) requireOnly(allowed ...string) error {
	set := map[string]bool{
		"playbook":   r.Playbook != "",
		"commit":     r.Commit != "",
		"limit":      r.Limit != "",
		"extra_vars": len(r.ExtraVars) > 0,
		"session":    r.Session != "",
		"workspace":  r.Workspace != "",
		"action":     r.Action != "",
		"targets":    len(r.Targets) > 0,
		"run":        r.Run != "",
		"follow":     r.Follow,
		"host":       r.Host != "",
		"reason":     r.Reason != "",
	}
	for _, name := range allowed {
		delete(set, name)
	}
	for name, present := range set {
		if present {
			return fmt.Errorf("a %s request does not use the field %s", r.Kind, name)
		}
	}
	return nil
}

func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
