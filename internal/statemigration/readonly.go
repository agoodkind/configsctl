package statemigration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
)

// ReadWorkspaces initializes a backend and lists its workspaces.
//
// The directory must contain only the trusted backend configuration.
//
// Example:
//
//	names, err := ReadWorkspaces(ctx, workspace, backendArgs)
func ReadWorkspaces(
	ctx context.Context,
	workspace Workspace,
	backendArgs []string,
) ([]string, error) {
	if err := initializeReadBackend(ctx, workspace, backendArgs); err != nil {
		return nil, err
	}

	var output bytes.Buffer
	if err := nativeWorkspace(
		ctx,
		workspace,
		"",
		&output,
		"workspace",
		"list",
		"-no-color",
	); err != nil {
		return nil, err
	}

	var names []string

	for line := range strings.SplitSeq(output.String(), "\n") {
		name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "* "))
		if name == "" {
			continue
		}

		if strings.ContainsAny(name, " /\\\t\r\x00") || name == "." || name == ".." ||
			slices.Contains(names, name) {
			return nil, errWorkspaceList
		}

		names = append(names, name)
	}

	if !slices.Contains(names, "default") {
		return nil, errDefaultWorkspace
	}

	if err := verifyWorkspaceList(ctx, workspace, names); err != nil {
		return nil, err
	}

	return names, nil
}

// ReadWorkspace checks the selected workspace before reading its state.
//
// Use a name from ReadWorkspaces and a new private capture path.
//
// Example:
//
//	state, err := ReadWorkspace(ctx, workspace, name, destination)
func ReadWorkspace(
	ctx context.Context,
	workspace Workspace,
	name string,
	destination string,
) ([]string, error) {
	var selected bytes.Buffer
	if err := nativeWorkspace(ctx, workspace, name, &selected, "workspace", "show"); err != nil {
		return nil, err
	}

	if strings.TrimSpace(selected.String()) != name {
		return nil, errWorkspaceSelection
	}

	if err := captureWorkspace(ctx, workspace, name, destination); err != nil {
		return nil, err
	}

	return readAddresses(destination)
}

var (
	errWorkspaceList      = errors.New("OpenTofu returned an invalid workspace list")
	errDefaultWorkspace   = errors.New("OpenTofu did not list the default workspace")
	errWorkspaceSelection = errors.New("OpenTofu selected a different workspace")
)

func readAddresses(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errStateAddressRead
	}

	state, err := decodeState(data)
	if err != nil {
		return nil, err
	}

	var addresses []string

	for _, resource := range state.Resources {
		address, err := baseAddress(resource)
		if err != nil {
			return nil, errInvalidResourceAddress
		}

		for _, instance := range resource.Instances {
			indexed, err := indexedAddress(address, instance["index_key"])
			if err != nil {
				return nil, errInvalidResourceAddress
			}

			addresses = append(addresses, indexed)
		}
	}

	return addresses, nil
}

var (
	errStateAddressRead       = errors.New("cannot read the private state capture")
	errInvalidResourceAddress = errors.New("private state contains an invalid resource address")
)
