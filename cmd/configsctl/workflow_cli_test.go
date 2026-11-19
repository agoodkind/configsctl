package main_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRetirementWorkflowPreparesAndVerifies(t *testing.T) {
	fixture := newWorkflowFixture(t)

	checkout, trusted := workflowGitFixture(t, fixture)
	for _, revision := range []string{fixture.current.Base.SHA, fixture.current.Head.SHA} {
		command := exec.CommandContext(
			t.Context(),
			"git",
			"-C",
			checkout,
			"cat-file",
			"-t",
			revision,
		)
		if command.Run() == nil {
			t.Fatal("The checkout already contains a commit that prepare must fetch.")
		}
	}

	result := fixture.run(t, checkout, "prepare", nil)
	requireCIExit(t, result, 0, "Retirement prepare")

	for _, revision := range []string{fixture.current.Base.SHA, fixture.current.Head.SHA} {
		if retirementRun(t, checkout, "git", "cat-file", "-t", revision) != "commit" {
			t.Fatal("Retirement prepare did not fetch the exact event commit.")
		}
	}

	if retirementRun(t, checkout, "git", "rev-parse", "HEAD") != trusted {
		t.Fatal("Retirement prepare changed the trusted HEAD.")
	}

	content, err := os.ReadFile(filepath.Join(checkout, "trusted.txt"))
	if err != nil || string(content) != "trusted checkout\n" {
		t.Fatal("Retirement prepare changed a trusted tracked file.")
	}

	if retirementRun(t, checkout, "git", "status", "--porcelain") != "" {
		t.Fatal("Retirement prepare modified the tracked checkout.")
	}

	result = fixture.run(
		t,
		checkout,
		"verify",
		map[string]string{
			"GITHUB_TOKEN":      "",
			"GITHUB_API_URL":    "",
			"GITHUB_SERVER_URL": "",
			"GITHUB_RUN_ID":     "",
		},
	)
	requireCIExit(t, result, 0, "Retirement verify without API or state credentials")
}

func TestRetirementWorkflowPublishesExactStatus(t *testing.T) {
	cases := []struct {
		name, verdict, verify, state string
	}{
		{"Both results succeed.", "success", "success", "success"},
		{"The verdict fails.", "failure", "success", "failure"},
		{"Verification fails.", "success", "failure", "failure"},
		{"The verdict is empty.", "", "success", "failure"},
		{"Verification is empty.", "success", "", "failure"},
		{"Verification is cancelled.", "success", "cancelled", "failure"},
		{"Verification is skipped.", "success", "skipped", "failure"},
		{"The verdict is skipped.", "skipped", "success", "failure"},
	}
	for _, test := range cases {
		t.Run(
			test.name,
			func(t *testing.T) {
				fixture := newWorkflowFixture(t)
				result := fixture.run(
					t,
					t.TempDir(),
					"publish",
					map[string]string{
						"VERDICT":       test.verdict,
						"VERIFY_RESULT": test.verify,
					},
				)
				requireCIExit(
					t,
					result,
					0,
					"Retirement publish without a checkout or state credentials",
				)
				publications := fixture.publications()
				if len(publications) != 1 {
					t.Fatalf(
						"Retirement publish created %d statuses instead of one.",
						len(publications),
					)
				}

				publication := publications[0]
				if publication.path != "/repos/example/configs/statuses/"+fixture.current.Head.SHA ||
					publication.status.State != test.state ||
					publication.status.Context != "retirement" ||
					publication.status.TargetURL != "https://github.example/example/configs/actions/runs/12345" {
					t.Fatalf("Retirement publish created an incorrect status: %+v", publication)
				}
			},
		)
	}
}

func TestRetirementWorkflowRejectsChangedPullRequest(t *testing.T) {
	cases := map[string]func(*workflowPull){
		"The base commit changed.": func(pull *workflowPull) {
			pull.Base.SHA = strings.Repeat("c", 40)
		},
		"The head commit changed.": func(pull *workflowPull) {
			pull.Head.SHA = strings.Repeat("c", 40)
		},
		"The pull request targets another branch.": func(pull *workflowPull) { pull.Base.Ref = "release" },
		"The pull request number differs.":         func(pull *workflowPull) { pull.Number++ },
		"The base repository differs.":             func(pull *workflowPull) { pull.Base.Repo.FullName = "other/configs" },
		"The API omits an immutable commit.":       func(pull *workflowPull) { pull.Head.SHA = "" },
	}
	for name, change := range cases {
		for _, mode := range []string{"prepare", "publish"} {
			t.Run(
				mode+"/"+name,
				func(t *testing.T) {
					fixture := newWorkflowFixture(t)
					change(&fixture.current)
					result := fixture.run(t, t.TempDir(), mode, nil)
					requireCIExit(t, result, 1, "Retirement with a changed pull request")
					if len(fixture.publications()) != 0 {
						t.Fatal("Retirement published a status for a changed pull request.")
					}
				},
			)
		}
	}
}

func TestRetirementWorkflowRejectsInvalidEvents(t *testing.T) {
	cases := map[string]func(*workflowEvent){
		"The event repository differs.":        func(event *workflowEvent) { event.Repository.FullName = "other/configs" },
		"The base repository differs.":         func(event *workflowEvent) { event.PullRequest.Base.Repo.FullName = "other/configs" },
		"The event number is missing.":         func(event *workflowEvent) { event.Number = 0 },
		"The pull request number is negative.": func(event *workflowEvent) { event.PullRequest.Number = -1 },
		"The base branch differs.":             func(event *workflowEvent) { event.PullRequest.Base.Ref = "release" },
		"The base is a branch name.":           func(event *workflowEvent) { event.PullRequest.Base.SHA = "main" },
		"The head is an abbreviated commit.":   func(event *workflowEvent) { event.PullRequest.Head.SHA = "bbbbbbb" },
	}
	for name, change := range cases {
		t.Run(
			name,
			func(t *testing.T) {
				fixture := newWorkflowFixture(t)
				change(&fixture.event)
				fixture.saveEvent(t)

				for _, mode := range []string{"prepare", "verify", "publish"} {
					requireCIExit(
						t,
						fixture.run(t, t.TempDir(), mode, nil),
						1,
						"Retirement with an invalid event",
					)
				}

				if len(fixture.publications()) != 0 {
					t.Fatal("Retirement published a status for an invalid event.")
				}
			},
		)
	}
}

func TestRetirementWorkflowRequiresRunnerInputs(t *testing.T) {
	for _, mode := range []string{"prepare", "verify", "publish"} {
		keys := []string{"GITHUB_EVENT_NAME", "GITHUB_EVENT_PATH", "GITHUB_REPOSITORY"}
		if mode != "verify" {
			keys = append(keys, "GITHUB_API_URL", "GITHUB_TOKEN")
		}

		if mode == "publish" {
			keys = append(keys, "GITHUB_SERVER_URL", "GITHUB_RUN_ID")
		}

		for _, key := range keys {
			t.Run(
				mode+"/"+key,
				func(t *testing.T) {
					fixture := newWorkflowFixture(t)
					result := fixture.run(t, t.TempDir(), mode, map[string]string{key: ""})
					requireCIExit(t, result, 1, "Retirement without a required input")
					if !strings.Contains(result.stdout, key) {
						t.Fatalf(
							"The command did not identify the missing %s input: %s",
							key,
							result.stdout,
						)
					}
				},
			)
		}
	}
}

func TestRetirementWorkflowRejectsMalformedInputs(t *testing.T) {
	cases := []map[string]string{
		{"GITHUB_EVENT_NAME": "pull_request"},
		{"GITHUB_EVENT_PATH": "/missing-retirement-event.json"},
		{"GITHUB_REPOSITORY": "../configs"},
		{"GITHUB_REPOSITORY": "example/configs?query"},
		{"GITHUB_API_URL": "file:///private"},
		{"GITHUB_API_URL": "https://user:" + workflowToken + "@github.example"},
		{"GITHUB_API_URL": "https://github.example?query=" + workflowToken},
		{"GITHUB_SERVER_URL": "https://github.example#fragment"},
		{"GITHUB_RUN_ID": "../42"},
	}
	for _, overrides := range cases {
		fixture := newWorkflowFixture(t)
		requireCIExit(
			t,
			fixture.run(t, t.TempDir(), "publish", overrides),
			1,
			"Retirement with malformed runner input",
		)
	}

	fixture := newWorkflowFixture(t)
	retirementSave(t, fixture.eventPath, []byte("private-api-response "+workflowToken))
	requireCIExit(
		t,
		fixture.run(t, t.TempDir(), "publish", nil),
		1,
		"Retirement with malformed event JSON",
	)
}

func TestRetirementWorkflowSanitizesAPIFailures(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusFound} {
		fixture := newWorkflowFixture(t)
		fixture.getCode = code
		fixture.response = "private-api-response " + workflowToken
		result := fixture.run(t, t.TempDir(), "publish", nil)
		requireCIExit(t, result, 1, "Retirement with a failed or redirected API request")
		if !strings.Contains(result.stdout, "HTTP "+strconv.Itoa(code)) {
			t.Fatalf("The command did not identify the HTTP failure: %s", result.stdout)
		}

		if len(fixture.publications()) != 0 {
			t.Fatal("Retirement published after a failed API request.")
		}
	}

	for _, response := range []string{"private-api-response " + workflowToken, "{} {}"} {
		fixture := newWorkflowFixture(t)
		fixture.response = response
		requireCIExit(
			t,
			fixture.run(t, t.TempDir(), "publish", nil),
			1,
			"Retirement with malformed API JSON",
		)
	}

	fixture := newWorkflowFixture(t)
	fixture.postCode = http.StatusForbidden
	requireCIExit(
		t,
		fixture.run(t, t.TempDir(), "publish", nil),
		1,
		"Retirement with a refused status publication",
	)
	fixture = newWorkflowFixture(t)
	fixture.server.Close()
	requireCIExit(
		t,
		fixture.run(t, t.TempDir(), "publish", nil),
		1,
		"Retirement with an unavailable API",
	)
}
