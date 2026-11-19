package main_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const workflowToken = "synthetic-retirement-token"

type workflowRepository struct {
	FullName string `json:"full_name"`
}

type workflowRevision struct {
	Ref  string             `json:"ref"`
	SHA  string             `json:"sha"`
	Repo workflowRepository `json:"repo"`
}

type workflowPull struct {
	Number int              `json:"number"`
	Base   workflowRevision `json:"base"`
	Head   workflowRevision `json:"head"`
}

type workflowEvent struct {
	Number      int                `json:"number"`
	Repository  workflowRepository `json:"repository"`
	PullRequest workflowPull       `json:"pull_request"`
}

type workflowStatus struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

type workflowPublication struct {
	path   string
	status workflowStatus
}

type workflowFixture struct {
	event       workflowEvent
	current     workflowPull
	server      *httptest.Server
	eventPath   string
	getCode     int
	postCode    int
	response    string
	mutex       sync.Mutex
	publication []workflowPublication
}

func newWorkflowFixture(t *testing.T) *workflowFixture {
	t.Helper()
	repository := workflowRepository{FullName: "example/configs"}
	pull := workflowPull{
		Number: 17,
		Base:   workflowRevision{Ref: "main", SHA: strings.Repeat("a", 40), Repo: repository},
		Head:   workflowRevision{Ref: "change", SHA: strings.Repeat("b", 40)},
	}
	fixture := &workflowFixture{
		event:     workflowEvent{Number: pull.Number, Repository: repository, PullRequest: pull},
		current:   pull,
		eventPath: filepath.Join(t.TempDir(), "event.json"),
		getCode:   http.StatusOK,
		postCode:  http.StatusCreated,
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serve))
	t.Cleanup(fixture.server.Close)
	fixture.saveEvent(t)

	return fixture
}

func (fixture *workflowFixture) saveEvent(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(fixture.event)
	if err != nil {
		t.Fatalf("The test cannot encode its event: %v", err)
	}

	retirementSave(t, fixture.eventPath, data)
}

func (fixture *workflowFixture) serve(writer http.ResponseWriter, request *http.Request) {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	if request.Header.Get("Authorization") != "Bearer "+workflowToken ||
		request.Header.Get("Accept") != "application/vnd.github+json" {
		writer.WriteHeader(http.StatusUnauthorized)

		return
	}

	if request.Method == http.MethodGet && request.URL.Path == "/repos/example/configs/pulls/17" {
		writer.Header().Set("Location", fixture.server.URL+"/redirected")
		writer.WriteHeader(fixture.getCode)
		if fixture.response != "" {
			_, _ = writer.Write([]byte(fixture.response))

			return
		}

		_ = json.NewEncoder(writer).Encode(fixture.current)

		return
	}

	if request.Method == http.MethodPost &&
		strings.HasPrefix(request.URL.Path, "/repos/example/configs/statuses/") {
		var status workflowStatus
		if json.NewDecoder(request.Body).Decode(&status) != nil {
			writer.WriteHeader(http.StatusBadRequest)

			return
		}

		fixture.publication = append(
			fixture.publication,
			workflowPublication{path: request.URL.Path, status: status},
		)
		writer.WriteHeader(fixture.postCode)
		_, _ = writer.Write([]byte("private-api-response " + workflowToken))

		return
	}

	writer.WriteHeader(http.StatusNotFound)
}

func (fixture *workflowFixture) run(
	t *testing.T,
	directory string,
	mode string,
	overrides map[string]string,
) commandResult {
	t.Helper()
	values := map[string]string{
		"PATH":                os.Getenv("PATH"),
		"HOME":                t.TempDir(),
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_TERMINAL_PROMPT": "0",
		"GITHUB_EVENT_NAME":   "pull_request_target",
		"GITHUB_EVENT_PATH":   fixture.eventPath,
		"GITHUB_REPOSITORY":   "example/configs",
		"GITHUB_API_URL":      fixture.server.URL,
		"GITHUB_TOKEN":        workflowToken,
		"GITHUB_SERVER_URL":   "https://github.example",
		"GITHUB_RUN_ID":       "12345",
		"VERDICT":             "success",
		"VERIFY_RESULT":       "success",
	}

	for key, value := range overrides {
		values[key] = value
	}

	command := exec.CommandContext(t.Context(), configsctlBinary, "ci", "retirement", mode)
	command.Dir = directory

	for key, value := range values {
		command.Env = append(command.Env, key+"="+value)
	}

	output, err := command.CombinedOutput()
	result := commandResult{stdout: string(output)}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.exitCode = exitError.ExitCode()
	} else if err != nil {
		t.Fatalf("The test cannot start configsctl: %v", err)
	}

	for _, private := range []string{workflowToken, "private-api-response"} {
		if strings.Contains(result.stdout, private) {
			t.Fatal("The retirement command disclosed a private API value.")
		}
	}

	return result
}

func (fixture *workflowFixture) publications() []workflowPublication {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()

	return append([]workflowPublication(nil), fixture.publication...)
}

func workflowGitFixture(t *testing.T, fixture *workflowFixture) (string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	origin := t.TempDir()
	retirementRun(t, origin, "git", "init", "-q", "-b", "main")
	retirementSave(
		t,
		filepath.Join(origin, "configsctl.yml"),
		[]byte("tofu:\n  workspaces_dir: opentofu\n"),
	)
	retirementSave(t, filepath.Join(origin, "trusted.txt"), []byte("trusted checkout\n"))
	trusted := retirementCommit(t, origin)
	checkout := filepath.Join(t.TempDir(), "checkout")
	retirementRun(t, origin, "git", "clone", "-q", "--no-local", origin, checkout)
	retirementSave(t, filepath.Join(origin, "base.txt"), []byte("new base\n"))
	fixture.event.PullRequest.Base.SHA = retirementCommit(t, origin)
	retirementSave(t, filepath.Join(origin, "trusted.txt"), []byte("untrusted head\n"))
	fixture.event.PullRequest.Head.SHA = retirementCommit(t, origin)
	retirementRun(
		t,
		origin,
		"git",
		"update-ref",
		"refs/pull/17/head",
		fixture.event.PullRequest.Head.SHA,
	)
	fixture.current = fixture.event.PullRequest
	fixture.saveEvent(t)

	return checkout, trusted
}
