package retirement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"goodkind.io/configsctl/internal/statemigration"
)

// Verify checks retired addresses in each affected base backend.
//
// Both revisions must be full Git commit IDs from the configuration checkout.
// Verify reads the base backend when the head removes a retirement instruction.
//
// Example:
//
//	base := "881ea0946db0f2a9990b35836fed00a00a5f355d"
//	head := "97d4b0f5c84f74157814b3d7b88b4d92cb001ac1"
//	if err := retirement.Verify(ctx, ".", base, head); err != nil {
//		return fmt.Errorf("cannot verify the configuration change: %w", err)
//	}
func Verify(ctx context.Context, checkout, base, head string) error {
	checkout, err := filepath.Abs(checkout)
	if err != nil {
		slog.Error("retirement.checkout_failed", "err", err)

		return fmt.Errorf("resolve configuration checkout: %w", err)
	}

	blobs := map[string][]byte{}
	before, err := readSnapshot(ctx, checkout, base, blobs)
	if err != nil {
		return err
	}

	after, err := readSnapshot(ctx, checkout, head, blobs)
	if err != nil {
		return err
	}

	workspaces, err := before.workspaceDir(ctx)
	if err != nil {
		return err
	}

	headWorkspaces, err := after.workspaceDir(ctx)
	if err != nil {
		return err
	}

	if workspaces == headWorkspaces && unchanged(ctx, before, after) {
		return nil
	}

	old, err := before.graph(ctx, workspaces)
	if err != nil {
		return err
	}

	current, err := after.graph(ctx, headWorkspaces)
	if err != nil {
		return err
	}

	return verifyChanges(ctx, checkout, before, after, old, current)
}

func verifyChanges(
	ctx context.Context,
	checkout string,
	before *snapshot,
	after *snapshot,
	old graph,
	current graph,
) error {
	for dir, unused := range old.unused {
		for target := range unused {
			if configuration := after.modules[dir]; configuration == nil ||
				!configuration.removed[target] {
				return fmt.Errorf("%w in %q", errUnknownConsumer, dir)
			}
		}
	}

	for _, dir := range slices.Sorted(maps.Keys(old.roots)) {
		var missing []hcl.Traversal
		headModule := after.modules[dir]
		retainedBackend := headModule != nil &&
			sameBackend(before.modules[dir].backend, headModule.backend)

		for target := range old.roots[dir] {
			if !current.roots[dir][target] ||
				!retainedBackend {
				traversal, diags := hclsyntax.ParseTraversalAbs(
					[]byte(target),
					"address",
					hcl.InitialPos,
				)
				if diags.HasErrors() {
					return errInvalidAddress
				}

				missing = append(missing, traversal)
			}
		}

		if len(missing) == 0 {
			continue
		}

		if err := verifyRoot(
			ctx,
			filepath.Join(checkout, dir),
			before.modules[dir],
			missing,
		); err != nil {
			slog.ErrorContext(ctx, "retirement.verification_failed", "directory", dir, "err", err)

			return fmt.Errorf("verify retired addresses for %q: %w", dir, err)
		}
	}

	return nil
}

func verifyRoot(
	ctx context.Context,
	original string,
	configuration *module,
	missing []hcl.Traversal,
) error {
	if configuration.encryption == nil {
		return errMissingEncryption
	}

	backend := configuration.backend.block.Labels[0]
	args, err := backendArguments(backend, original, configuration.backend)
	if err != nil {
		return err
	}

	if backend == "s3" &&
		(os.Getenv("AWS_ACCESS_KEY_ID") == "" || os.Getenv("AWS_SECRET_ACCESS_KEY") == "") {
		return errMissingAWSAccess
	}

	if configuration.stateVariable != nil && os.Getenv("TF_VAR_vault_tofu_state_passphrase") == "" {
		return errMissingStateVariable
	}

	dir, err := os.MkdirTemp("", "configsctl-retirement-*")
	if err != nil {
		slog.Error("retirement.temp_directory_failed", "err", err)

		return fmt.Errorf("create private backend directory: %w", err)
	}

	runErr := inspectBackend(ctx, dir, configuration, missing, args)

	return errors.Join(runErr, os.RemoveAll(dir))
}

func inspectBackend(
	ctx context.Context,
	dir string,
	configuration *module,
	missing []hcl.Traversal,
	args []string,
) error {
	for index, block := range []*sourceBlock{
		configuration.backend,
		configuration.encryption,
		configuration.stateVariable,
	} {
		if block == nil {
			continue
		}

		if err := block.write(dir, index); err != nil {
			return err
		}
	}

	env := []string{
		"HOME=" + dir,
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=" + os.DevNull,
		"AWS_SHARED_CREDENTIALS_FILE=" + os.DevNull,
	}

	for _, key := range []string{
		"PATH",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"TF_VAR_vault_tofu_state_passphrase",
	} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}

	workspace := statemigration.Workspace{Directory: dir, Environment: env}
	names, err := statemigration.ReadWorkspaces(ctx, workspace, args)
	if err != nil {
		slog.ErrorContext(ctx, "retirement.backend_init_failed", "err", err)

		return fmt.Errorf("initialize and enumerate the base backend: %w", err)
	}

	for i, name := range names {
		capture := filepath.Join(dir, "capture-"+strconv.Itoa(i))
		addresses, err := statemigration.ReadWorkspace(ctx, workspace, name, capture)
		if err != nil {
			return fmt.Errorf("workspace %q %w: %w", name, errUnreadableState, err)
		}

		if err := checkRecords(name, addresses, missing); err != nil {
			return err
		}

		if err := os.Remove(capture); err != nil {
			return errPrivateCapture
		}
	}

	return nil
}

func backendArguments(kind, original string, block *sourceBlock) ([]string, error) {
	if kind == "s3" {
		return nil, nil
	}

	if kind != "local" {
		return nil, errUnsupportedBackend
	}

	attrs, diag := block.block.Body.JustAttributes()
	if diag.HasErrors() {
		return nil, errLocalBackend
	}

	defaults := map[string]string{
		"path":          "terraform.tfstate",
		"workspace_dir": "terraform.tfstate.d",
	}
	var args []string

	for _, key := range []string{"path", "workspace_dir"} {
		value := defaults[key]
		if attr := attrs[key]; attr != nil {
			v, d := attr.Expr.Value(nil)
			if d.HasErrors() || v.IsNull() || v.Type() != cty.String {
				return nil, errLocalPath
			}

			value = v.AsString()
		}

		if !filepath.IsAbs(value) {
			value = filepath.Join(original, value)
		}

		args = append(args, "-backend-config="+key+"="+value)
	}

	return args, nil
}

func (block *sourceBlock) write(dir string, index int) error {
	filename := filepath.Join(dir, "state-"+strconv.Itoa(index)+".tf")
	var data []byte
	if block.native != nil {
		data = block.native.Range().SliceBytes(block.data)
		if blockKind(block.block.Type) != variableBlock {
			data = []byte("terraform {\n" + string(data) + "\n}\n")
		}
	} else {
		var err error
		data, err = block.jsonBlock()
		if err != nil {
			return err
		}

		filename += ".json"
	}

	if err := os.WriteFile(filename, data, 0o600); err != nil {
		slog.Error("retirement.backend_projection_failed", "err", err)

		return fmt.Errorf("write private backend configuration: %w", err)
	}

	return nil
}

func (block *sourceBlock) jsonBlock() ([]byte, error) {
	attrs, diag := block.block.Body.JustAttributes()
	if diag.HasErrors() {
		return nil, errBackendJSON
	}

	body := map[string]json.RawMessage{}

	for key, attr := range attrs {
		body[key] = attr.Expr.Range().SliceBytes(block.data)
	}

	value, err := json.Marshal(body)
	if err != nil {
		return nil, errEncodeBackend
	}

	levels := slices.Clone(block.block.Labels)
	slices.Reverse(levels)
	levels = append(levels, block.block.Type)
	if blockKind(block.block.Type) != variableBlock {
		levels = append(levels, string(terraformBlock))
	}

	for _, level := range levels {
		value, err = json.Marshal(map[string]json.RawMessage{level: value})
		if err != nil {
			return nil, errEncodeBackend
		}
	}

	return value, nil
}

func checkRecords(name string, addresses []string, missing []hcl.Traversal) error {
	for _, record := range addresses {
		traversal, diags := hclsyntax.ParseTraversalAbs([]byte(record), "state", hcl.InitialPos)
		if diags.HasErrors() {
			return errStateAddress
		}

		for _, target := range missing {
			if matches(target, traversal) {
				slog.Error("retirement.address_present", "err", errRetainedAddress)

				return fmt.Errorf("workspace %q %w", name, errRetainedAddress)
			}
		}
	}

	return nil
}

var (
	errInvalidAddress    = errors.New("invalid removed address")
	errMissingEncryption = errors.New("the base backend requires an encryption block")
	errMissingAWSAccess  = errors.New(
		"supply read-only AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY",
	)
	errMissingStateVariable = errors.New("supply TF_VAR_vault_tofu_state_passphrase")
	errPrivateCapture       = errors.New("remove the private state capture")
	errUnsupportedBackend   = errors.New("state verification supports local and S3 backends")
	errLocalBackend         = errors.New("invalid local backend configuration")
	errLocalPath            = errors.New("local backend paths must be literal strings")
	errBackendJSON          = errors.New("invalid base backend JSON block")
	errEncodeBackend        = errors.New("encode the base backend configuration")
	errStateAddress         = errors.New("cannot verify an instance address in private state")
	errUnreadableState      = errors.New("requires an existing readable state")
	errRetainedAddress      = errors.New("still contains a retired address")
	errUnknownConsumer      = errors.New("cannot identify a base backend for a removed instruction")
)
