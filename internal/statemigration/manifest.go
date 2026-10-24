package statemigration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const manifestVersion = 1

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Manifest assigns every existing instance to exactly one reviewed destination.
type Manifest struct {
	Version                int             `json:"version"`
	Commit                 string          `json:"commit"`
	RestoreCommit          string          `json:"restore_commit"`
	ConfigurationDirectory string          `json:"configuration_directory"`
	Workspaces             []WorkspaceSpec `json:"workspaces"`
	Moves                  []Transfer      `json:"moves"`
}

// WorkspaceSpec identifies a backend and the exact state expected before transfer.
type WorkspaceSpec struct {
	Selector string    `json:"selector"`
	Backend  string    `json:"backend"`
	Before   StateInfo `json:"before"`
}

// Transfer changes ownership of one complete native state instance.
type Transfer struct {
	Source      string `json:"source"`
	Address     string `json:"address"`
	Destination string `json:"destination"`
	NewAddress  string `json:"new_address"`
}

// DecodeManifest refuses unknown fields and incomplete or duplicate ownership.
func DecodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("decode the versioned migration manifest with its required fields")
	}
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("the migration manifest must contain one JSON document")
	}
	return manifest, manifest.Validate()
}

// Validate checks the complete mapping against the expected source instance set.
func (m Manifest) Validate() error {
	if err := m.validateConfiguration(); err != nil {
		return err
	}
	return m.validateOwnership()
}

func (m Manifest) validateConfiguration() error {
	if m.Version != manifestVersion || !commitPattern.MatchString(m.Commit) || !commitPattern.MatchString(m.RestoreCommit) {
		return errors.New("the migration manifest requires version 1 and full reviewed cutover and recovery commits")
	}
	if !filepath.IsAbs(m.ConfigurationDirectory) || filepath.Clean(m.ConfigurationDirectory) != m.ConfigurationDirectory {
		return errors.New("the migration manifest requires configuration_directory as a clean absolute path to the retained cutover checkout")
	}
	if len(m.Workspaces) == 0 || len(m.Moves) == 0 {
		return errors.New("the migration manifest requires workspaces and a complete instance mapping")
	}
	return nil
}

func (m Manifest) validateOwnership() error {
	workspaces := make(map[string]WorkspaceSpec)
	backends := make(map[string]bool)
	remaining := make(map[string]bool)
	for _, workspace := range m.Workspaces {
		if err := validSelector(workspace.Selector); err != nil {
			return err
		}
		if _, exists := workspaces[workspace.Selector]; exists || workspace.Backend == "" || backends[workspace.Backend] {
			return fmt.Errorf("workspace %s duplicates a selector or backend identity", workspace.Selector)
		}
		if workspace.Before.Lineage == "" && (workspace.Before.Serial != 0 || len(workspace.Before.Records) != 0) {
			return fmt.Errorf("new workspace %s declares existing state metadata without a lineage", workspace.Selector)
		}
		workspaces[workspace.Selector] = workspace
		backends[workspace.Backend] = true
		for address := range workspace.Before.Records {
			remaining[workspace.Selector+"\x00"+address] = true
		}
	}
	return m.validateTransfers(workspaces, remaining)
}

func (m Manifest) validateTransfers(workspaces map[string]WorkspaceSpec, remaining map[string]bool) error {
	destinations := make(map[string]bool)
	for _, move := range m.Moves {
		if _, exists := workspaces[move.Source]; !exists {
			return fmt.Errorf("the mapping references undeclared source %s", move.Source)
		}
		if _, exists := workspaces[move.Destination]; !exists {
			return fmt.Errorf("the mapping references undeclared destination %s", move.Destination)
		}
		if err := validAddress(move.Address); err != nil {
			return err
		}
		if err := validAddress(move.NewAddress); err != nil {
			return err
		}
		source := move.Source + "\x00" + move.Address
		destination := move.Destination + "\x00" + move.NewAddress
		if !remaining[source] {
			return fmt.Errorf("source instance %s in %s is absent or mapped more than once", move.Address, move.Source)
		}
		if destinations[destination] {
			return fmt.Errorf("destination instance %s in %s has more than one owner", move.NewAddress, move.Destination)
		}
		delete(remaining, source)
		destinations[destination] = true
	}
	if len(remaining) != 0 {
		return fmt.Errorf("the migration manifest omits %d existing state instances", len(remaining))
	}
	return nil
}

func validSelector(selector string) error {
	if selector == "" || path.IsAbs(selector) || path.Clean(selector) != selector || strings.Contains(selector, "\\") {
		return fmt.Errorf("migration selector %q must be a clean relative workspace path", selector)
	}
	if selector == "." {
		return nil
	}
	for component := range strings.SplitSeq(selector, "/") {
		if strings.HasPrefix(component, ".") || component == "modules" || component == "terraform.d" {
			return fmt.Errorf("migration selector %q contains an excluded directory", selector)
		}
	}
	return nil
}

func validAddress(address string) error {
	if _, diags := hclsyntax.ParseTraversalAbs([]byte(address), "migration address", hcl.InitialPos); diags.HasErrors() {
		return errors.New("the migration mapping contains an invalid native instance address")
	}
	return nil
}

func (m Manifest) fingerprint() (string, error) {
	encoded, err := json.Marshal(m)
	if err != nil {
		slog.Error("migration.manifest.encode_failed", "err", migrationErrorType(err))
		return "", fmt.Errorf("encode the reviewed migration manifest metadata: %w", err)
	}
	return digest(encoded), nil
}
