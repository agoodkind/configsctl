// Package tofuplansource binds source identity to exact saved OpenTofu plan bytes.
package tofuplansource

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Source records Git or installed-artifact identity for a selected workspace.
// Empty optional fields preserve compatibility with existing Git plan records.
type Source struct {
	Status         string `json:"status"`
	Commit         string `json:"commit"`
	Workspace      string `json:"workspace"`
	Kind           string `json:"kind,omitempty"`
	Root           string `json:"root,omitempty"`
	Artifact       string `json:"artifact,omitempty"`
	Node           string `json:"node,omitempty"`
	Environment    string `json:"environment,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
}

// Path computes the cache path from the current saved-plan bytes.
func Path(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		slog.Error("tofu.source.plan_stat_failed", "err", err)
		return "", fmt.Errorf("read the saved plan %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the saved plan %s is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("tofu.source.plan_read_failed", "err", err)
		return "", fmt.Errorf("read the recorded plan %s: %w", path, err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		slog.Error("tofu.source.cache_unavailable", "err", err)
		return "", fmt.Errorf("find the saved plan source cache: %w", err)
	}
	return filepath.Join(cache, "configsctl", "plan-source", fmt.Sprintf("%x.json", sha256.Sum256(data))), nil
}

// Record stores the source of a plan without writing into the configuration tree.
func Record(path string, source Source) error {
	slog.Debug("tofu.source.plan_record", "kind", source.Kind)
	metadata, err := Path(path)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		slog.Error("tofu.source.plan_encode_failed", "err", err)
		return fmt.Errorf("encode the saved plan source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(metadata), 0o700); err != nil {
		slog.Error("tofu.source.cache_create_failed", "err", err)
		return fmt.Errorf("create the saved plan source cache: %w", err)
	}
	if err := os.WriteFile(metadata, encoded, 0o600); err != nil {
		slog.Error("tofu.source.plan_record_failed", "err", err)
		return fmt.Errorf("record the saved plan source: %w", err)
	}
	return nil
}

// Remove deletes a cached source record for stale plan bytes.
func Remove(path string) error {
	metadata, err := Path(path)
	if err != nil {
		return err
	}
	// A new saved plan may not have a cached source record yet.
	if err := os.Remove(metadata); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("tofu.source.plan_record_remove_failed", "err", err)
		return fmt.Errorf("remove the stale saved plan source record: %w", err)
	}
	return nil
}

// Require compares the current source with the record for these exact plan bytes.
func Require(path string, current Source) error {
	metadata, err := Path(path)
	if err != nil {
		return err
	}
	encoded, err := os.ReadFile(metadata)
	if err != nil {
		slog.Error("tofu.source.plan_record_unavailable", "err", err)
		return fmt.Errorf("regenerate the saved plan with configsctl tofu plan; its source record is unavailable: %w", err)
	}
	var source Source
	if err := json.Unmarshal(encoded, &source); err != nil {
		slog.Error("tofu.source.plan_record_invalid", "err", err)
		return fmt.Errorf("read the saved plan source record: %w", err)
	}
	if source.Status != "committed" || current.Status != "committed" || source != current {
		if current.Kind == "installed" {
			return errors.New("regenerate the saved plan from the current installed artifact; its recorded source does not match")
		}
		return errors.New("regenerate the saved plan from this clean merged configuration commit; its recorded source does not match")
	}
	return nil
}
