package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configs/internal/ansible"
	"goodkind.io/configs/internal/redact"
)

// readOnlyRunLog returns the single file under the run log root, with its
// contents. It fails the test unless exactly one log exists, so a test that
// expects one run cannot pass against a stray second file.
func readOnlyRunLog(t *testing.T) (path, contents string) {
	t.Helper()
	entries, err := os.ReadDir(runLogRoot)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", runLogRoot, err)
	}
	if len(entries) != 1 {
		t.Fatalf("run log dir holds %d entries, want 1", len(entries))
	}
	path = filepath.Join(runLogRoot, entries[0].Name())
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return path, string(body)
}

// TestRunDeployWritesThePlayOutputToTheLogFile is the whole point of the log:
// what the play writes lands on disk, in full, no matter what the caller does
// with the tool's own stdout.
func TestRunDeployWritesThePlayOutputToTheLogFile(t *testing.T) {
	t.Chdir(t.TempDir())
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
	if err := runDeployWith(cmdEnv{}, []string{"deploy-mwan"}, nil, deploy); err != nil {
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
	t.Chdir(t.TempDir())
	secret := "supersecrettokenvalue0123456789"
	env := cmdEnv{secrets: []redact.Pattern{{Value: []byte(secret), Label: "vault_token"}}}
	deploy := func(opts ansible.DeployOptions) error {
		_, err := opts.Output.Write([]byte("ok: [mwan] token=" + secret + "\n"))
		return err
	}
	if err := runDeployWith(env, []string{"deploy-mwan"}, nil, deploy); err != nil {
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
	t.Chdir(t.TempDir())
	deploy := func(opts ansible.DeployOptions) error {
		if _, err := opts.Output.Write([]byte("fatal: [mwan]: UNREACHABLE!\n")); err != nil {
			return err
		}
		return errors.New("ansible-playbook: exit status 4")
	}
	err := runDeployWith(cmdEnv{}, []string{"deploy-mwan"}, nil, deploy)
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

// TestOpenRunLogPermissions pins that a log is readable only by the operator who
// ran the command. A diffing run prints file contents into it.
func TestOpenRunLogPermissions(t *testing.T) {
	t.Chdir(t.TempDir())
	log, err := openRunLog("deploy-mwan", nil)
	if err != nil {
		t.Fatalf("openRunLog: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(log.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != fs.FileMode(runLogPerm) {
		t.Fatalf("log mode = %v, want %v", got, fs.FileMode(runLogPerm))
	}
}

// TestCreateRunLogFileNeverOverwrites pins that two runs started within the same
// second get two files. The timestamp resolves to one second, so without the
// counter the second run would either fail or clobber the first run's output
// while an operator was still reading it.
func TestCreateRunLogFileNeverOverwrites(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(runLogRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	const stamp = "20260819T151126Z"
	names := map[string]bool{}
	for run := 1; run <= 3; run++ {
		file, err := createRunLogFile("deploy-mwan", stamp)
		if err != nil {
			t.Fatalf("createRunLogFile run %d: %v", run, err)
		}
		if _, err := file.WriteString("run\n"); err != nil {
			t.Fatalf("write run %d: %v", run, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close run %d: %v", run, err)
		}
		if names[file.Name()] {
			t.Fatalf("run %d reused the name %s", run, file.Name())
		}
		names[file.Name()] = true
	}

	entries, err := os.ReadDir(runLogRoot)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("run log dir holds %d files after 3 runs, want 3", len(entries))
	}
}

// TestRunLogName pins that every accepted label, stem or path, reduces to the
// same filename-safe name.
func TestRunLogName(t *testing.T) {
	tests := []struct {
		arg  string
		want string
	}{
		{arg: "deploy-mwan", want: "deploy-mwan"},
		{arg: "playbooks/deploy-mwan.yml", want: "deploy-mwan"},
		{arg: "ansible/playbooks/deploy-testbed.yaml", want: "deploy-testbed"},
		{arg: "tofu-plan", want: "tofu-plan"},
		{arg: "deploy mwan", want: "deploy-mwan"},
		{arg: "../../etc/passwd", want: "passwd"},
		{arg: "/", want: "run"},
	}
	for _, tc := range tests {
		t.Run(tc.arg, func(t *testing.T) {
			if got := runLogName(tc.arg); got != tc.want {
				t.Fatalf("runLogName(%q) = %q, want %q", tc.arg, got, tc.want)
			}
		})
	}
}

// TestTofuStreams pins which tofu invocations write to a run log. A run that
// stops at an approval prompt must keep the terminal, because the prompt is the
// output and an operator cannot answer a question they cannot see.
func TestTofuStreams(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "plan streams", args: []string{"plan"}, want: true},
		{name: "plan with flags streams", args: []string{"-chdir=x", "plan", "-detailed-exitcode"}, want: true},
		{name: "refresh streams", args: []string{"refresh"}, want: true},
		{name: "apply prompts", args: []string{"apply"}, want: false},
		{name: "apply auto-approve streams", args: []string{"apply", "-auto-approve"}, want: true},
		{name: "apply double dash auto-approve streams", args: []string{"apply", "--auto-approve"}, want: true},
		{name: "apply auto-approve with value streams", args: []string{"apply", "-auto-approve=true"}, want: true},
		{name: "destroy prompts", args: []string{"destroy"}, want: false},
		{name: "destroy auto-approve streams", args: []string{"destroy", "-auto-approve"}, want: true},
		{name: "console keeps the terminal", args: []string{"console"}, want: false},
		{name: "init keeps the terminal", args: []string{"init"}, want: false},
		{name: "output keeps the terminal", args: []string{"output", "-json"}, want: false},
		{name: "unknown subcommand keeps the terminal", args: []string{"whatever"}, want: false},
		{name: "no subcommand keeps the terminal", args: []string{"-help"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tofuStreams(tc.args); got != tc.want {
				t.Fatalf("tofuStreams(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestTofuSubcommand pins that the run log name comes from the subcommand, not
// from a leading global flag.
func TestTofuSubcommand(t *testing.T) {
	tests := []struct {
		args []string
		want tofuCommand
	}{
		{args: []string{"plan"}, want: tofuPlan},
		{args: []string{"-chdir=opentofu", "apply"}, want: tofuApply},
		{args: []string{"-help"}, want: ""},
		{args: nil, want: ""},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if got := tofuSubcommand(tc.args); got != tc.want {
				t.Fatalf("tofuSubcommand(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
