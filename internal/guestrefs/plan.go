package guestrefs

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"goodkind.io/configsctl/internal/tofulock"
)

// Registry collects guest declarations and references from provider-free plans.
type Registry map[string]workspace

// New enables collection when a repository requests guest validation.
//
// A nil registry still reads ordinary operational lock targets.
//
// Example:
//
//	registry := New(settings.ValidateGuestReferences)
func New(enabled bool) Registry {
	if enabled {
		return make(Registry)
	}

	return nil
}

// Read decodes lock targets from a provider-free OpenTofu plan.
//
// When validation is enabled, Read also collects the guest references.
//
// Example:
//
//	targets, err := registry.Read(repo, root, "suburban/host", body)
func (registry Registry) Read(
	repository string,
	renderRoot string,
	selector string,
	body []byte,
) (tofulock.Targets, error) {
	var targets tofulock.Targets
	var document struct {
		PlannedValues struct {
			Outputs map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"outputs"`
		} `json:"planned_values"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		slog.Error("tofu.configuration.invalid_plan", "workspace", selector, "err", err)

		return targets, fmt.Errorf("decode the native configuration projection: %w", err)
	}

	output, found := document.PlannedValues.Outputs["lock_targets"]
	if !found {
		return targets, validationError(
			"native configuration for " + selector + " must export lock_targets",
		)
	}

	if err := json.Unmarshal(output.Value, &targets); err != nil {
		slog.Error("tofu.configuration.invalid_lock_targets", "workspace", selector, "err", err)

		return targets, fmt.Errorf("decode lock_targets for %s: %w", selector, err)
	}

	if registry == nil {
		return targets, nil
	}

	metadata, found := document.PlannedValues.Outputs["guest_references"]
	if !found {
		return targets, validationError(selector + " must export guest_references")
	}

	var refs workspace
	if err := json.Unmarshal(metadata.Value, &refs); err != nil {
		slog.Error("tofu.configuration.invalid_references", "workspace", selector, "err", err)

		return targets, fmt.Errorf("decode guest_references for %s: %w", selector, err)
	}

	if refs.Source != "" {
		lines, err := sourceLocations(repository, renderRoot, refs.Source)
		if err != nil {
			return targets, err
		}

		refs.lines = lines
	}

	refs.Targets = targets
	registry[selector] = refs

	return targets, nil
}
