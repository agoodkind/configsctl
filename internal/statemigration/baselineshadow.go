package statemigration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

type shadowPlanSource struct {
	Workspace Workspace
	Expected  StateInfo
	Snapshot  bool
}

func (e Engine) shadowPlan(ctx context.Context, scratch, checkout, configuration string, endpoint Workspace, source shadowPlanSource, target planReportTarget) ([]byte, error) {
	if err := target.Write(newPlanReport(target.Workspace)); err != nil {
		return nil, err
	}
	reviewed, err := filepath.EvalSymlinks(configuration)
	if err != nil {
		return nil, errors.New("resolve the reviewed baseline configuration directory")
	}
	selected, err := filepath.EvalSymlinks(endpoint.Directory)
	if err != nil {
		return nil, errors.New("resolve the reviewed baseline workspace directory")
	}
	relative, err := filepath.Rel(reviewed, selected)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, errors.New("the baseline workspace must remain inside its reviewed configuration checkout")
	}
	workspace := Workspace{Directory: filepath.Join(checkout, relative), Environment: endpoint.Environment}
	state := filepath.Join(scratch, target.Name+".tfstate")
	quoted := hclwrite.TokensForValue(cty.StringVal(state)).Bytes()
	override := bytes.ReplaceAll(migrationOverrideTemplate, []byte(migrationOverridePlaceholder), quoted)
	if err := writePreflightOverride(ctx, workspace.Directory, override); err != nil {
		return nil, err
	}
	if err := Initialize(ctx, workspace); err != nil {
		return nil, err
	}
	if err := requirePreflightLocalBackend(ctx, workspace.Directory, state); err != nil {
		return nil, err
	}
	if err := populateShadowState(ctx, scratch, workspace, source, target.Name); err != nil {
		return nil, err
	}
	return readIndependentPlan(ctx, workspace, scratch, target)
}

func populateShadowState(ctx context.Context, scratch string, workspace Workspace, source shadowPlanSource, name string) error {
	plaintext := filepath.Join(scratch, "capture-"+name+".tfstate")
	actual, err := Pull(ctx, source.Workspace, plaintext)
	if errors.Is(err, ErrAbsent) && absentStateMetadata(source.Expected) {
		return nil
	}
	if err != nil {
		return err
	}
	verified := sameState(actual, source.Expected)
	if source.Snapshot {
		verified = samePayload(actual, source.Expected) && actual.Serial >= source.Expected.Serial && actual.Serial-source.Expected.Serial <= 2
	}
	if !verified {
		return errors.New("the private baseline state differs from its immutable verified checkpoint")
	}
	if err := Push(ctx, workspace, plaintext); err != nil {
		slog.ErrorContext(ctx, "migration.plan_baseline.shadow_push_failed", "err", migrationErrorType(err))
		return fmt.Errorf("populate the private baseline backend: %w", err)
	}
	return nil
}
