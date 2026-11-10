package statemigration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/statemigration"
)

const syntheticMarker = "synthetic-migration-value-never-print-72d4"

var nativeEncryptionKey = rand.Text()

func workspaceFixture(t *testing.T, name string) statemigration.Workspace {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", name+".tf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "main.tf"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := statemigration.Workspace{
		Directory: path,
		Environment: append(os.Environ(),
			fmt.Sprintf("TF_VAR_vault_tofu_state_passphrase=%s", nativeEncryptionKey),
			"TF_VAR_fixture_value="+syntheticMarker),
	}
	nativeCommand(t, workspace, "init", "-input=false", "-no-color")
	return workspace
}

func nativeCommand(t *testing.T, workspace statemigration.Workspace, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "tofu", args...)
	cmd.Dir = workspace.Directory
	cmd.Env = workspace.Environment
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("native fixture %s failed: %v", args[0], err)
	}
}

func privateFixtureDir(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeEncryptedTransferPreservesInstanceAndRefusesStalePush(t *testing.T) {
	ctx := context.Background()
	source := workspaceFixture(t, "source")
	destination := workspaceFixture(t, "destination")
	nativeCommand(t, source, "apply", "-auto-approve", "-input=false", "-no-color")
	private := privateFixtureDir(t)
	sourceExport := filepath.Join(private, "source.tfstate")
	before, err := statemigration.Pull(ctx, source, sourceExport)
	if err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(private, "before")
	snapshot, err := statemigration.Snapshot(ctx, source, sourceExport, snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	requireNativeSnapshotPermissions(t, snapshotPath)
	restored, err := statemigration.Pull(ctx, snapshot, filepath.Join(private, "restored.tfstate"))
	if err != nil || restored.Digest != before.Digest {
		t.Fatalf("the encrypted snapshot changed resource verification metadata: %v", err)
	}
	destinationExport := filepath.Join(private, "destination.tfstate")
	if err := statemigration.Move(ctx, source, sourceExport, destinationExport, "terraform_data.original", "terraform_data.renamed"); err != nil {
		t.Fatal(err)
	}
	if err := statemigration.Push(ctx, destination, destinationExport); err != nil {
		t.Fatal(err)
	}
	after, err := statemigration.Pull(ctx, destination, filepath.Join(private, "after.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Records) != 1 || before.Records["terraform_data.original"] != after.Records["terraform_data.renamed"] {
		t.Fatal("native transfer changed the resource identity or provider state")
	}
	nativeCommand(t, destination, "plan", "-input=false", "-detailed-exitcode", "-no-color")
	requirePrivateNativeFiles(t, private, "source.tfstate", "destination.tfstate", "restored.tfstate", "after.tfstate")
	// A second real update advances the destination serial before the stale push.
	destination.Environment = append(destination.Environment, "TF_VAR_fixture_value=synthetic-later-update")
	nativeCommand(t, destination, "apply", "-auto-approve", "-input=false", "-no-color")
	err = statemigration.Push(ctx, destination, destinationExport)
	if err == nil {
		t.Fatal("native push overwrote a later destination state")
	}
	if strings.Contains(err.Error(), syntheticMarker) {
		t.Fatal("native failure exposed the synthetic state value")
	}
}

func requireNativeSnapshotPermissions(t *testing.T, directory string) {
	t.Helper()
	encrypted, err := os.ReadFile(filepath.Join(directory, "snapshot.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(syntheticMarker)) {
		t.Fatal("the retained snapshot contains the synthetic plaintext marker")
	}
	requirePrivateNativeFiles(t, directory, "snapshot.tfstate", "main.tf", filepath.Join(".terraform", "terraform.tfstate"))
	metadata, err := os.Stat(filepath.Join(directory, ".terraform"))
	if err != nil || metadata.Mode().Perm() != 0o700 {
		t.Fatal("retained snapshot backend metadata requires a private directory")
	}
}

func requirePrivateNativeFiles(t *testing.T, directory string, names ...string) {
	t.Helper()
	for _, name := range names {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("private native file %s must have mode 0600: %v", name, err)
		}
	}
}

func TestPrivateCaptureRefusesSymlinkAndExistingOutput(t *testing.T) {
	ctx := context.Background()
	workspace := workspaceFixture(t, "source")
	nativeCommand(t, workspace, "apply", "-auto-approve", "-input=false", "-no-color")
	private := privateFixtureDir(t)
	path := filepath.Join(private, "captured.tfstate")
	if _, err := statemigration.Pull(ctx, workspace, path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.Pull(ctx, workspace, path); err == nil {
		t.Fatal("native capture overwrote an existing private export")
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := statemigration.Pull(ctx, workspace, filepath.Join(link, "escape.tfstate")); err == nil {
		t.Fatal("native capture accepted a symlink as its private directory")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("a refused private capture changed the existing export")
	}
}
