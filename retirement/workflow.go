package retirement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strconv"
)

var runID = regexp.MustCompile(`^[1-9][0-9]*$`)

const resultSuccess = "success"

var (
	errFetch = errors.New(
		"the retirement command cannot fetch the base commit and pull request head from origin",
	)
	errFetchCommits = errors.New("the retirement fetch did not provide both exact event commits")
	errRunID        = errors.New("GITHUB_RUN_ID must contain a positive workflow run ID")
	errStatusJSON   = errors.New("the retirement command cannot encode the commit status")
)

func prepareGitHub(ctx context.Context, event githubEvent, client *githubClient) error {
	if err := client.confirm(ctx, event); err != nil {
		return err
	}

	_, err := git(
		ctx,
		".",
		"fetch",
		"--no-tags",
		"--no-write-fetch-head",
		"origin",
		event.PullRequest.Base.SHA,
		"refs/pull/"+strconv.Itoa(event.Number)+"/head",
	)
	if err != nil {
		return errFetch
	}

	for _, revision := range []string{event.PullRequest.Base.SHA, event.PullRequest.Head.SHA} {
		kind, err := git(ctx, ".", "cat-file", "-t", revision)
		if err != nil || string(kind) != "commit\n" {
			return errFetchCommits
		}
	}

	return nil
}

type githubStatus struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
}

func publishGitHub(ctx context.Context, event githubEvent, client *githubClient) error {
	server, err := runnerURL("GITHUB_SERVER_URL")
	if err != nil {
		return err
	}

	run, err := requiredRunner("GITHUB_RUN_ID")
	if err != nil {
		return err
	}

	if !runID.MatchString(run) {
		return errRunID
	}

	if err := client.confirm(ctx, event); err != nil {
		return err
	}

	status := githubStatus{
		State:       "failure",
		Context:     "retirement",
		Description: "Retirement verification did not succeed.",
		TargetURL:   server + "/" + client.repository + "/actions/runs/" + run,
	}
	if os.Getenv("VERDICT") == resultSuccess && os.Getenv("VERIFY_RESULT") == resultSuccess {
		status.State = resultSuccess
		status.Description = "Retirement verification succeeded."
	}

	data, err := json.Marshal(status)
	if err != nil {
		return errStatusJSON
	}

	response, err := client.request(
		ctx,
		http.MethodPost,
		"/statuses/"+event.PullRequest.Head.SHA,
		bytes.NewReader(data),
		http.StatusCreated,
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	return nil
}

func githubCommand(ctx context.Context, mode string) error {
	event, repository, err := readGitHubEvent()
	if err != nil {
		return err
	}

	if mode == "verify" {
		return Verify(ctx, ".", event.PullRequest.Base.SHA, event.PullRequest.Head.SHA)
	}

	client, err := newGitHubClient(repository)
	if err != nil {
		return err
	}

	if mode == "prepare" {
		return prepareGitHub(ctx, event, &client)
	}

	return publishGitHub(ctx, event, &client)
}
