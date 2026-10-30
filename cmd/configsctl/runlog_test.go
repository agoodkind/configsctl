package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/ansible"
	"goodkind.io/configsctl/internal/redact"
	"goodkind.io/configsctl/internal/runlog"
)

// privateTempDir assigns a test-owned host temp directory. runlog.Root reads
// os.TempDir on every invocation, and os.TempDir reads TMPDIR first. Each test
// uses its own run logs instead of the operator's logs.
func privateTempDir(t *testing.T) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
}

// readOnlyRunLog returns the single file under the run log root, with its
// contents. It fails the test unless exactly one log exists, so a test that
// expects one run cannot pass against a stray second file.
func readOnlyRunLog(t *testing.T) (path, contents string) {
	t.Helper()
	root := runlog.Root()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", root, err)
	}
	if len(entries) != 1 {
		t.Fatalf("run log dir holds %d entries, want 1", len(entries))
	}
	path = filepath.Join(root, entries[0].Name())
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return path, string(body)
}

// TestRunDeployLeavesNoLogInTheWorkingDirectory pins that a run leaves no
// artifact in the working tree. The operator runs these commands from a git
// checkout, and a log written there would show up in status and in a build.
func TestRunDeployLeavesNoLogInTheWorkingDirectory(t *testing.T) {
	privateTempDir(t)
	checkout := t.TempDir()
	t.Chdir(checkout)
	deploy := func(opts ansible.DeployOptions) error {
		_, err := opts.Output.Write([]byte("ok: [mwan]\n"))
		return err
	}
	if err := runDeployWith(cmdEnv{}, []string{"deploy-mwan"}, deploy); err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}

	entries, err := os.ReadDir(checkout)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", checkout, err)
	}
	if len(entries) != 0 {
		t.Fatalf("the working directory holds %d entries after a deploy, want none", len(entries))
	}
	path, _ := readOnlyRunLog(t)
	if !strings.HasPrefix(path, os.TempDir()) {
		t.Fatalf("run log %q is not under the host temp dir %q", path, os.TempDir())
	}
}

// TestRunDeployWritesThePlayOutputToTheLogFile is the whole point of the log:
// what the play writes lands on disk, in full, no matter what the caller does
// with the tool's own stdout.
func TestRunDeployWritesThePlayOutputToTheLogFile(t *testing.T) {
	privateTempDir(t)
	play := "TASK [install packages] ****\nok: [mwan]\nPLAY RECAP ****\n"
	deploy := func(opts ansible.DeployOptions) error {
		if opts.Output == nil {
			t.Fatal("deploy received no Output writer")
		}
		if _, err := opts.Output.Write([]byte(play)); err != nil {
			t.Fatalf("play write: %v", err)
		}
		return nil
	}
	if err := runDeployWith(cmdEnv{}, []string{"deploy-mwan"}, deploy); err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}

	path, contents := readOnlyRunLog(t)
	if contents != play {
		t.Fatalf("log contents = %q, want %q", contents, play)
	}
	if !strings.HasPrefix(filepath.Base(path), "deploy-mwan-") {
		t.Fatalf("log name = %q, want it to lead with the playbook stem", filepath.Base(path))
	}
	if !strings.HasSuffix(path, ".log") {
		t.Fatalf("log name = %q, want a .log suffix", path)
	}
}

// TestRunDeployRedactsSecretsInTheLogFile pins that the log is covered by the
// same redaction the terminal gets. The file is a durable artifact, so a secret
// the play echoes must not reach it.
func TestRunDeployRedactsSecretsInTheLogFile(t *testing.T) {
	privateTempDir(t)
	secret := "supersecrettokenvalue0123456789"
	env := cmdEnv{secrets: []redact.Pattern{{Value: []byte(secret), Label: "vault_token"}}}
	deploy := func(opts ansible.DeployOptions) error {
		_, err := opts.Output.Write([]byte("ok: [mwan] token=" + secret + "\n"))
		return err
	}
	if err := runDeployWith(env, []string{"deploy-mwan"}, deploy); err != nil {
		t.Fatalf("runDeployWith: %v", err)
	}

	_, contents := readOnlyRunLog(t)
	if strings.Contains(contents, secret) {
		t.Fatalf("the log holds the secret value: %q", contents)
	}
	want := "ok: [mwan] token=<redacted:vault_token>\n"
	if contents != want {
		t.Fatalf("log contents = %q, want %q", contents, want)
	}
}

// TestRunDeployFlushesTheLogWhenThePlayFails pins that a failed play still
// leaves its output on disk, and that the returned error names the log so the
// operator does not have to hunt for it.
func TestRunDeployFlushesTheLogWhenThePlayFails(t *testing.T) {
	privateTempDir(t)
	deploy := func(opts ansible.DeployOptions) error {
		if _, err := opts.Output.Write([]byte("fatal: [mwan]: UNREACHABLE!\n")); err != nil {
			return err
		}
		return errors.New("ansible-playbook: exit status 4")
	}
	err := runDeployWith(cmdEnv{}, []string{"deploy-mwan"}, deploy)
	if err == nil {
		t.Fatal("runDeployWith returned nil for a failing play")
	}

	path, contents := readOnlyRunLog(t)
	if contents != "fatal: [mwan]: UNREACHABLE!\n" {
		t.Fatalf("log contents = %q, want the play output", contents)
	}
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		t.Fatalf("Abs(%s): %v", path, absErr)
	}
	if !strings.Contains(err.Error(), absolute) {
		t.Fatalf("error = %v, want it to name %s", err, absolute)
	}
}
