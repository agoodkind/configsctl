package statemigration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
)

type engineSession struct {
	state         sync.Mutex
	canceled      <-chan struct{}
	ownership     Engine
	operationDone chan struct{}
	active        bool
	closed        bool
}

// WithSession retains one executor and exclusive writer lease across migration stages.
// The callback uses the supplied engine and context without changing their configuration.
func (e Engine) WithSession(ctx context.Context, callback func(Engine) error) error {
	if ctx == nil || callback == nil {
		return errors.New("a migration session requires a context and callback")
	}
	if e.session != nil {
		return errors.New("a captured migration session engine cannot start another session")
	}
	if err := context.Cause(ctx); err != nil {
		slog.ErrorContext(ctx, "migration.session.start_canceled", "err", migrationErrorType(err))
		return fmt.Errorf("start the migration session: %w", err)
	}
	owned := copySessionEngine(e)
	releaseExecutor, err := owned.acquire()
	if err != nil {
		return err
	}
	defer releaseExecutor()
	releaseLease, err := owned.acquirePauseLease()
	if err != nil {
		return err
	}
	defer releaseLease()
	owned.session = &engineSession{
		state: sync.Mutex{}, canceled: ctx.Done(), ownership: copySessionEngine(owned),
		operationDone: nil, active: false, closed: false,
	}
	defer owned.session.close()
	if err := owned.session.validate(owned); err != nil {
		return err
	}
	callbackErr := callback(owned)
	owned.session.close()
	if err := context.Cause(ctx); err != nil {
		slog.ErrorContext(ctx, "migration.session.stopped", "err", migrationErrorType(err))
		return errors.Join(callbackErr, fmt.Errorf("the migration session stopped: %w", err))
	}
	return callbackErr
}

func copySessionEngine(engine Engine) Engine {
	engine.session = nil
	engine.Manifest.Moves = slices.Clone(engine.Manifest.Moves)
	engine.Manifest.Workspaces = slices.Clone(engine.Manifest.Workspaces)
	for index, workspace := range engine.Manifest.Workspaces {
		workspace.Before.Records = maps.Clone(workspace.Before.Records)
		workspace.Before.Dependencies = maps.Clone(workspace.Before.Dependencies)
		for address, dependencies := range workspace.Before.Dependencies {
			workspace.Before.Dependencies[address] = slices.Clone(dependencies)
		}
		engine.Manifest.Workspaces[index] = workspace
	}
	engine.Workspaces = copySessionWorkspaces(engine.Workspaces)
	engine.OriginalWorkspaces = copySessionWorkspaces(engine.OriginalWorkspaces)
	engine.RestoreWorkspaces = copySessionWorkspaces(engine.RestoreWorkspaces)
	return engine
}

func copySessionWorkspaces(workspaces map[string]Workspace) map[string]Workspace {
	copied := maps.Clone(workspaces)
	for selector, workspace := range copied {
		workspace.Environment = slices.Clone(workspace.Environment)
		copied[selector] = workspace
	}
	return copied
}

func (session *engineSession) acquire(engine Engine) (func(), error) {
	session.state.Lock()
	defer session.state.Unlock()
	if err := session.validateLocked(engine); err != nil {
		return nil, err
	}
	if session.active {
		return nil, errors.New("another operation is using this migration session")
	}
	done := make(chan struct{})
	session.operationDone = done
	session.active = true
	return func() {
		session.state.Lock()
		session.active = false
		session.operationDone = nil
		close(done)
		session.state.Unlock()
	}, nil
}

func (session *engineSession) validate(engine Engine) error {
	session.state.Lock()
	defer session.state.Unlock()
	return session.validateLocked(engine)
}

func (session *engineSession) validateLocked(engine Engine) error {
	if session.closed {
		return errors.New("the captured migration session is closed; start a new session with the original engine")
	}
	select {
	case <-session.canceled:
		slog.Error("migration.session.operation_canceled", "err", migrationErrorType(context.Canceled))
		return fmt.Errorf("the migration session stopped: %w", context.Canceled)
	default:
	}
	if !sameSessionEngine(engine, session.ownership) {
		return errors.New("the migration session engine differs from its immutable ownership")
	}
	return nil
}

func (session *engineSession) close() {
	session.state.Lock()
	session.closed = true
	done, active := session.operationDone, session.active
	session.state.Unlock()
	if active {
		<-done
	}
}

func sameSessionEngine(actual, expected Engine) bool {
	return actual.CurrentCommit == expected.CurrentCommit && actual.Directory == expected.Directory && actual.PauseFile == expected.PauseFile && actual.PlanOptions == expected.PlanOptions &&
		sameSessionManifest(actual.Manifest, expected.Manifest) && sameSessionWorkspaces(actual.Workspaces, expected.Workspaces) &&
		sameSessionWorkspaces(actual.OriginalWorkspaces, expected.OriginalWorkspaces) && sameSessionWorkspaces(actual.RestoreWorkspaces, expected.RestoreWorkspaces)
}

func sameSessionManifest(actual, expected Manifest) bool {
	if actual.Version != expected.Version || actual.Commit != expected.Commit || actual.RestoreCommit != expected.RestoreCommit ||
		actual.ConfigurationDirectory != expected.ConfigurationDirectory || actual.RestoreConfigurationDirectory != expected.RestoreConfigurationDirectory ||
		(actual.Moves == nil) != (expected.Moves == nil) || !slices.Equal(actual.Moves, expected.Moves) ||
		(actual.Workspaces == nil) != (expected.Workspaces == nil) {
		return false
	}
	return slices.EqualFunc(actual.Workspaces, expected.Workspaces, func(left, right WorkspaceSpec) bool {
		return left.Selector == right.Selector && left.Backend == right.Backend && sameSessionStateInfo(left.Before, right.Before)
	})
}

func sameSessionStateInfo(actual, expected StateInfo) bool {
	if actual.Lineage != expected.Lineage || actual.Serial != expected.Serial || actual.Digest != expected.Digest || actual.Content != expected.Content ||
		(actual.Records == nil) != (expected.Records == nil) || !maps.Equal(actual.Records, expected.Records) ||
		(actual.Dependencies == nil) != (expected.Dependencies == nil) {
		return false
	}
	return maps.EqualFunc(actual.Dependencies, expected.Dependencies, func(left, right []string) bool {
		return (left == nil) == (right == nil) && slices.Equal(left, right)
	})
}

func sameSessionWorkspaces(actual, expected map[string]Workspace) bool {
	return (actual == nil) == (expected == nil) && maps.EqualFunc(actual, expected, func(left, right Workspace) bool {
		return left.Directory == right.Directory && (left.Environment == nil) == (right.Environment == nil) && slices.Equal(left.Environment, right.Environment)
	})
}
