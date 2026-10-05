package gate_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/gate"
)

// controllerClone creates a bare origin with one commit on main and a clone of
// it with one more local commit. It returns the clone and both commits.
func controllerClone(t *testing.T) (clone, onMain, offMain string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "clone")
	git(t, root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	git(t, root, "clone", "--quiet", origin, clone)
	commitFile(t, clone, "ansible/playbooks/deploy-tack.yml")
	git(t, clone, "push", "--quiet", "origin", "HEAD:main")
	onMain = strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	commitFile(t, clone, "ansible/playbooks/local-only.yml")
	offMain = strings.TrimSpace(git(t, clone, "rev-parse", "HEAD"))
	return clone, onMain, offMain
}

func commitFile(t *testing.T, repo, path string) {
	t.Helper()
	full := filepath.Join(repo, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(full, []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	git(t, repo, "add", path)
	git(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false",
		"-c", "core.hooksPath=/dev/null", "commit", "--quiet", "-m", "add "+path)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func serve(t *testing.T, clone, sshCommand, request string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	server := gate.Server{
		Repo:       gate.Repo{Dir: clone},
		Runs:       gate.Runs{Dir: filepath.Join(t.TempDir(), "runs")},
		Requester:  "claude",
		Controller: "controller-test",
		Self:       "/usr/local/bin/configsctl",
		Out:        &out,
	}
	err := server.Serve(t.Context(), sshCommand, strings.NewReader(request))
	return out.String(), err
}

func TestGateRefusesEverySSHCommandExceptRequest(t *testing.T) {
	clone, _, _ := controllerClone(t)
	for _, command := range []string{"", "bash", "request; id", "ls -la"} {
		if _, err := serve(t, clone, command, `{"kind":"runs"}`); err == nil {
			t.Fatalf("ssh command %q: err = nil, want a refusal", command)
		}
	}
}

func TestGateRefusesInvalidRequests(t *testing.T) {
	clone, onMain, _ := controllerClone(t)
	refused := map[string]string{
		"unknown field":     `{"kind":"runs","shell":"id"}`,
		"unknown kind":      `{"kind":"exec"}`,
		"playbook path":     `{"kind":"deploy","playbook":"../../etc/passwd","commit":"` + onMain + `","session":"s1"}`,
		"short commit":      `{"kind":"deploy","playbook":"deploy-tack","commit":"abc123","session":"s1"}`,
		"no session":        `{"kind":"deploy","playbook":"deploy-tack","commit":"` + onMain + `"}`,
		"extra vars array":  `{"kind":"deploy","playbook":"deploy-tack","commit":"` + onMain + `","session":"s1","extra_vars":["a"]}`,
		"field of another":  `{"kind":"status","run":"20261005T050000Z-aaaaaaaa","playbook":"deploy-tack"}`,
		"tofu destroy":      `{"kind":"tofu","workspace":"guest","action":"destroy","commit":"` + onMain + `","session":"s1"}`,
		"two JSON values":   `{"kind":"runs"}{"kind":"runs"}`,
		"unlock no reason":  `{"kind":"unlock","host":"tack-qa","run":"20261005T050000Z-aaaaaaaa"}`,
		"limit with spaces": `{"kind":"deploy","playbook":"deploy-tack","commit":"` + onMain + `","session":"s1","limit":"a b"}`,
	}
	for name, request := range refused {
		if _, err := serve(t, clone, "request", request); err == nil {
			t.Fatalf("%s: err = nil, want a refusal", name)
		}
	}
}

func TestGateRefusesACommitThatIsNotOnOriginMain(t *testing.T) {
	clone, _, offMain := controllerClone(t)
	request := `{"kind":"deploy","playbook":"local-only","commit":"` + offMain + `","session":"s1"}`
	_, err := serve(t, clone, "request", request)
	if err == nil || !strings.Contains(err.Error(), "is not on origin/main") {
		t.Fatalf("err = %v, want the origin/main refusal", err)
	}
}

func TestGateReportsAMissingRun(t *testing.T) {
	clone, _, _ := controllerClone(t)
	_, err := serve(t, clone, "request", `{"kind":"status","run":"20261005T050000Z-aaaaaaaa"}`)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want the missing run error", err)
	}
	out, err := serve(t, clone, "request", `{"kind":"runs"}`)
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("runs: out = %q, err = %v; want an empty list", out, err)
	}
}
