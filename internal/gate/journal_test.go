package gate_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/gate"
)

type journalLine struct {
	Time       string          `json:"time"`
	Controller string          `json:"controller"`
	Requester  string          `json:"requester"`
	Session    string          `json:"session"`
	Kind       string          `json:"kind"`
	Run        string          `json:"run"`
	Playbook   string          `json:"playbook"`
	ExtraVars  json.RawMessage `json:"extra_vars"`
	Result     string          `json:"result"`
	Error      string          `json:"error"`
}

func serveInto(t *testing.T, clone, runsDir, sshCommand, request string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	server := gate.Server{
		Repo:              gate.Repo{Dir: clone},
		Runs:              gate.Runs{Dir: runsDir},
		Requester:         "claude",
		Controller:        "controller-test",
		Self:              "/usr/local/bin/configsctl",
		Out:               &out,
		VaultPasswordFile: vaultPasswordFile(clone),
	}
	err := server.Serve(t.Context(), sshCommand, strings.NewReader(request))
	return out.String(), err
}

func readJournal(t *testing.T, runsDir string) []journalLine {
	t.Helper()
	path := filepath.Join(runsDir, "requests.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, want 0600", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	lines := []journalLine{}
	for raw := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		var line journalLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decode journal line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func deployRequest(commit string) string {
	return `{"kind":"deploy","playbook":"deploy-tack","commit":"` + commit +
		`","session":"s1","extra_vars":{"token":"` + deployToken + `"}}`
}

func TestGateJournalsAStatusRequestAndARefusedRequest(t *testing.T) {
	clone, _, _ := controllerClone(t)
	runsDir := filepath.Join(t.TempDir(), "runs")
	missing := "20261005T050000Z-aaaaaaaa"
	if _, err := serveInto(t, clone, runsDir, "bash", `{"kind":"runs"}`); err == nil {
		t.Fatal("ssh command bash: err = nil, want a refusal")
	}
	if _, err := serveInto(t, clone, runsDir, "request", `{"kind":"status","run":"`+missing+`"}`); err == nil {
		t.Fatal("status of a missing run: err = nil, want the missing run error")
	}
	out, err := serveInto(t, clone, runsDir, "request", `{"kind":"runs"}`)
	if err != nil || strings.TrimSpace(out) != "[]" {
		t.Fatalf("runs: out = %q, err = %v; want an empty list beside the journal", out, err)
	}
	lines := readJournal(t, runsDir)
	if len(lines) != 3 {
		t.Fatalf("journal has %d lines, want 3: %+v", len(lines), lines)
	}
	for _, line := range lines {
		if line.Time == "" || line.Controller != "controller-test" || line.Requester != "claude" {
			t.Fatalf("journal line lacks time, controller, or requester: %+v", line)
		}
	}
	refused, status, list := lines[0], lines[1], lines[2]
	if refused.Kind != "" || refused.Result != "error" || !strings.Contains(refused.Error, "runs only") {
		t.Fatalf("refused line = %+v, want an error line without a kind", refused)
	}
	if status.Kind != "status" || status.Run != missing || status.Result != "error" || !strings.Contains(status.Error, "does not exist") {
		t.Fatalf("status line = %+v, want the missing run error", status)
	}
	if list.Kind != "runs" || list.Result != "ok" || list.Error != "" {
		t.Fatalf("runs line = %+v, want an ok line", list)
	}
}

func TestGateStoresMaskedExtraVars(t *testing.T) {
	clone, onMain, _ := controllerClone(t)
	runsDir := filepath.Join(t.TempDir(), "runs")
	if _, err := serveInto(t, clone, runsDir, "request", deployRequest(onMain)); err != nil {
		t.Logf("deploy request ended with %v", err)
	}
	lines := readJournal(t, runsDir)
	if len(lines) != 1 {
		t.Fatalf("journal has %d lines, want 1: %+v", len(lines), lines)
	}
	line := lines[0]
	if line.Kind != "deploy" || line.Session != "s1" || line.Playbook != "deploy-tack" || line.Run == "" {
		t.Fatalf("deploy line = %+v, want kind, session, playbook, and run", line)
	}
	requireMasked(t, "journal extra_vars", line.ExtraVars)
	record, err := gate.Runs{Dir: runsDir}.Read(line.Run)
	if err != nil {
		t.Fatalf("read record of %s: %v", line.Run, err)
	}
	requireMasked(t, "record extra_vars", record.Request.ExtraVars)
	for _, path := range []string{filepath.Join(runsDir, "requests.jsonl"), filepath.Join(runsDir, line.Run, "record.json")} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(body), deployToken) {
			t.Fatalf("%s contains the secret value", path)
		}
	}
}

func TestGateMasksVaultSecretsInALogsAnswer(t *testing.T) {
	clone, onMain, _ := controllerClone(t)
	runsDir := filepath.Join(t.TempDir(), "runs")
	if _, err := serveInto(t, clone, runsDir, "request", deployRequest(onMain)); err != nil {
		t.Logf("deploy request ended with %v", err)
	}
	run := readJournal(t, runsDir)[0].Run
	playLog := filepath.Join(runsDir, run, "tmp", "configs-runs", "play.log")
	if err := os.MkdirAll(filepath.Dir(playLog), 0o700); err != nil {
		t.Fatalf("create run log directory: %v", err)
	}
	if err := os.WriteFile(playLog, []byte("token="+deployToken+"\n"), 0o600); err != nil {
		t.Fatalf("write run log: %v", err)
	}
	out, err := serveInto(t, clone, runsDir, "request", `{"kind":"logs","run":"`+run+`"}`)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(out, "token=<redacted:vault_deploy_token>") || strings.Contains(out, deployToken) {
		t.Fatalf("logs answer = %q, want the token masked", out)
	}
}

func TestGateRefusesADeployWhenTheVaultIsMissing(t *testing.T) {
	clone, onMain, _ := controllerClone(t)
	if err := os.Remove(vaultPath(clone)); err != nil {
		t.Fatalf("remove vault: %v", err)
	}
	runsDir := filepath.Join(t.TempDir(), "runs")
	out, err := serveInto(t, clone, runsDir, "request", deployRequest(onMain))
	if err == nil || !strings.Contains(err.Error(), "vault secret patterns") {
		t.Fatalf("err = %v, want the missing vault refusal", err)
	}
	if out != "" {
		t.Fatalf("answer = %q, want none", out)
	}
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		t.Fatalf("read runs directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "requests.jsonl" {
		t.Fatalf("runs directory has %v, want only the journal", entries)
	}
	lines := readJournal(t, runsDir)
	if len(lines) != 1 {
		t.Fatalf("journal has %d lines, want 1: %+v", len(lines), lines)
	}
	line := lines[0]
	if line.Kind != "deploy" || line.Session != "s1" || line.Result != "error" || line.Run != "" || line.ExtraVars != nil {
		t.Fatalf("journal line = %+v, want a deploy error line without a run or extra_vars", line)
	}
}

func requireMasked(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	var extraVars map[string]string
	if err := json.Unmarshal(raw, &extraVars); err != nil {
		t.Fatalf("decode %s %s: %v", name, raw, err)
	}
	if extraVars["token"] != "<redacted:vault_deploy_token>" || strings.Contains(string(raw), deployToken) {
		t.Fatalf("%s = %s, want the token masked", name, raw)
	}
}
