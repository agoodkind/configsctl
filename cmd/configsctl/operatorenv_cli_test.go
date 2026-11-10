//go:build unix

package main_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func operatorAgent(t *testing.T, home string) string {
	t.Helper()
	directory, err := os.MkdirTemp(shortTempRoot, "operator-agent-*")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	log, err := os.CreateTemp(directory, "agent-log-*")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = log.Close() })
	socket := filepath.Join(directory, "agent.sock")
	ctx, stop := context.WithCancel(t.Context())
	command := exec.CommandContext(ctx, "ssh-agent", "-D", "-a", socket)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { stop(); _ = command.Wait() })
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return socket
		}

		select {
		case <-deadline.C:
			body, err := os.ReadFile(log.Name())
			t.Fatalf("the fixture agent did not create its socket: %v\n%s", err, body)
		case <-ticker.C:
		}
	}
}

func TestNativeExecutionPreservesBoundedOperatorInputs(t *testing.T) {
	fixture := newDirectFixture(t, explicitSettings)
	installNativeConfiguration(t, fixture.tree)
	if err := os.Chmod(fixture.tree.home, 0o755); err != nil {
		t.Fatal(err)
	}

	socket := operatorAgent(t, fixture.tree.home)
	writeFixtureFile(
		t,
		filepath.Join(fixture.tree.home, ".config/native-fixture/input"),
		"operator-profile",
	)
	root := filepath.Join(fixture.tree.workspaces, "suburban", "tack")
	writeFixtureFile(t, filepath.Join(root, "main.tf"), nativeFixtureFile(t, "operator-root.tf"))
	writeFixtureFile(
		t,
		filepath.Join(root, "item/main.tf"),
		nativeFixtureFile(t, "operator-item.tf"),
	)
	writeFixtureFile(
		t,
		filepath.Join(root, "item/operator-inputs.sh"),
		nativeFixtureFile(t, "operator-inputs.sh"),
	)
	fixture.extraEnv = []string{
		"SSH_AUTH_SOCK=" + socket,
		"TF_VAR_operator_value=declared-operator-value",
		"TF_VAR_operator_empty=",
		"TF_VAR_operator_sensitive=protected-operator-fixture",
		"TF_VAR_operator_ephemeral=transient-operator-fixture",
		"TF_VAR_not_declared=unrelated-input",
		"UNRELATED_CANARY=unrelated-ambient-value",
	}
	diagnostic := filepath.Join(t.TempDir(), "operator-events.json")
	result := fixture.apply(t, "-auto-approve", "-json-into="+diagnostic)
	requireSuccess(t, result)
	actual, err := os.ReadFile(filepath.Join(root, "item/operator-result.txt"))
	if err != nil {
		t.Fatal(err)
	}

	home, err := filepath.EvalSymlinks(fixture.tree.home)
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		home,
		socket,
		"operator-profile",
		"declared-operator-value",
		"",
		"transient-operator-fixture",
	}
	if !slices.Equal(strings.Split(strings.TrimSuffix(string(actual), "\n"), "\n"), expected) {
		t.Fatalf("native execution changed the declared operator inputs: %q", actual)
	}

	events, err := os.ReadFile(diagnostic)
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []string{"protected-operator-fixture", "transient-operator-fixture"} {
		if strings.Contains(result.stdout+result.stderr+string(events), value) {
			t.Fatal("native output disclosed a protected operator input")
		}
	}

	refused := fixture.apply(
		t,
		"-auto-approve",
		"-var=operator_sensitive=protected-operator-fixture",
	)
	if refused.exitCode == 0 ||
		strings.Contains(refused.stdout+refused.stderr, "protected-operator-fixture") {
		t.Fatalf(
			"native secret argument outcome: exit=%d, plaintext=%t, details=%s",
			refused.exitCode,
			strings.Contains(refused.stdout+refused.stderr, "protected-operator-fixture"),
			strings.ReplaceAll(refused.stderr, "protected-operator-fixture", "<registered-input>"),
		)
	}

	info, err := os.Stat(fixture.tree.home)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("native execution changed the operator HOME permissions")
	}

	if err := os.Chmod(fixture.tree.home, 0o777); err != nil {
		t.Fatal(err)
	}

	unsafe := fixture.apply(t, "-auto-approve")
	if unsafe.exitCode == 0 ||
		!strings.Contains(
			unsafe.stderr,
			"manual HOME must not allow group or other users to write",
		) {
		t.Fatal("native execution accepted a writable operator HOME")
	}
}
