// Package version reports the running configsctl binary's build identity.
// go-makefile stamps Commit, Version, Dirty, and BuildTime at link time through
// VPKG, both for local builds and for released archives. This package adds a
// hash of the binary as it sits on disk and prints both in the one-line form
// mwan uses for its own build identity.
package version

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// The stamped build fields. go-build.mk's VPKG contract requires all four, and
// an unstamped build leaves them empty.
var (
	Version   string
	Commit    string
	Dirty     string
	BuildTime string
)

const unknown = "unknown"

// binaryHashLength is how many hex characters of the binary's SHA-256 the
// version line prints.
const binaryHashLength = 12

// dirtyStamp is the value the Dirty field carries. The pipeline writes a
// boolean word; the "dirty" and "clean" spellings are accepted so a binary
// stamped by hand still reports correctly.
type dirtyStamp string

const (
	dirtyStampTrue  dirtyStamp = "true"
	dirtyStampFalse dirtyStamp = "false"
	dirtyStampDirty dirtyStamp = "dirty"
	dirtyStampClean dirtyStamp = "clean"
)

// BuildVersionString returns the one-line build identity:
//
//	commit=<commit> dirty=<dirty|clean|unknown> binhash=<12 hex>
func BuildVersionString() string {
	return fmt.Sprintf("commit=%s dirty=%s binhash=%s", GitCommit(), GitDirty(), BinaryHash())
}

// GitCommit returns the stamped commit, or "unknown" when nothing was stamped.
func GitCommit() string {
	if Commit == "" {
		return unknown
	}
	return Commit
}

// GitDirty returns "dirty", "clean", or "unknown".
func GitDirty() string {
	switch dirtyStamp(Dirty) {
	case dirtyStampTrue, dirtyStampDirty:
		return "dirty"
	case dirtyStampFalse, dirtyStampClean:
		return "clean"
	default:
		return unknown
	}
}

// BinaryHash returns the first 12 hex characters of the SHA-256 of the running
// binary, or "unknown" when the binary cannot be read.
func BinaryHash() string {
	path, err := os.Executable()
	if err != nil {
		slog.Warn("version executable path failed", "err", err)
		return unknown
	}
	return binaryHashFrom(path)
}

// binaryHashFrom hashes the file at path.
func binaryHashFrom(path string) string {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("version binary open failed", "err", err)
		return unknown
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		slog.Warn("version binary hash failed", "err", err)
		return unknown
	}
	return hex.EncodeToString(digest.Sum(nil))[:binaryHashLength]
}
