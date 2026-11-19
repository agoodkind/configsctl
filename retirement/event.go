package retirement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	githubTimeout       = 30 * time.Second
	githubResponseLimit = 2097152
)

var repositoryID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

var (
	errRunnerRequired = errors.New("the retirement command requires a runner variable")
	errEventName      = errors.New("the retirement command requires a pull_request_target event")
	errRepository     = errors.New("GITHUB_REPOSITORY must identify an owner and repository")
	errEventRead      = errors.New("the retirement command cannot read GITHUB_EVENT_PATH")
	errEventJSON      = errors.New("the retirement event must contain valid pull request JSON")
	errEventIdentity  = errors.New(
		"the retirement event has an invalid pull request or repository identity",
	)
	errPullIdentity = errors.New(
		"the retirement pull request has an invalid number or base repository",
	)
	errPullBase    = errors.New("the retirement pull request must target main")
	errPullCommits = errors.New(
		"the retirement pull request requires full immutable Git commit IDs",
	)
	errRunnerURL = errors.New(
		"the runner URL must use HTTP or HTTPS without credentials, query, or fragment",
	)
	errAPIRequest   = errors.New("the retirement command cannot construct the GitHub API request")
	errAPITransport = errors.New(
		"the retirement GitHub API request failed or exceeded its timeout",
	)
	errAPIStatus = errors.New("the retirement GitHub API request failed")
	errAPIJSON   = errors.New(
		"the retirement GitHub pull request response must contain valid JSON",
	)
	errAPITrailing = errors.New(
		"the retirement GitHub pull request response contains trailing data",
	)
	errStalePull = errors.New(
		"the current pull request differs from the retirement event. Run the workflow again",
	)
)

type githubRepository struct {
	FullName string `json:"full_name"`
}

type githubRevision struct {
	Ref  string           `json:"ref"`
	SHA  string           `json:"sha"`
	Repo githubRepository `json:"repo"`
}

type githubPull struct {
	Number int            `json:"number"`
	Base   githubRevision `json:"base"`
	Head   githubRevision `json:"head"`
}

type githubEvent struct {
	Number      int              `json:"number"`
	Repository  githubRepository `json:"repository"`
	PullRequest githubPull       `json:"pull_request"`
}

type githubClient struct {
	api        string
	token      string
	repository string
	client     http.Client
}

func requiredRunner(name string) (string, error) {
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		slog.Error("retirement.runner_input_missing", "variable", name, "err", errRunnerRequired)

		return "", fmt.Errorf("%w: %s", errRunnerRequired, name)
	}

	return value, nil
}

func readGitHubEvent() (githubEvent, string, error) {
	var event githubEvent
	name, err := requiredRunner("GITHUB_EVENT_NAME")
	if err != nil {
		return event, "", err
	}

	if name != "pull_request_target" {
		return event, "", errEventName
	}

	file, err := requiredRunner("GITHUB_EVENT_PATH")
	if err != nil {
		return event, "", err
	}

	repository, err := requiredRunner("GITHUB_REPOSITORY")
	if err != nil {
		return event, "", err
	}

	if !repositoryID.MatchString(repository) || strings.HasSuffix(repository, "/.") ||
		strings.HasSuffix(repository, "/..") {
		return event, "", errRepository
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return event, "", errEventRead
	}

	if json.Unmarshal(data, &event) != nil {
		return event, "", errEventJSON
	}

	if event.Number <= 0 || event.Number != event.PullRequest.Number ||
		event.Repository.FullName != repository {
		return event, "", errEventIdentity
	}

	if err := event.PullRequest.validate(repository); err != nil {
		return event, "", err
	}

	return event, repository, nil
}

func (pull githubPull) validate(repository string) error {
	if pull.Number <= 0 || pull.Base.Repo.FullName != repository {
		return errPullIdentity
	}

	if pull.Base.Ref != "main" {
		return errPullBase
	}

	if !commitID.MatchString(pull.Base.SHA) || !commitID.MatchString(pull.Head.SHA) {
		return errPullCommits
	}

	return nil
}

func runnerURL(name string) (string, error) {
	value, err := requiredRunner(name)
	if err != nil {
		return "", err
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") {
		slog.Error("retirement.runner_url_invalid", "variable", name, "err", errRunnerURL)

		return "", fmt.Errorf("%w: %s", errRunnerURL, name)
	}

	return strings.TrimRight(value, "/"), nil
}

func newGitHubClient(repository string) (githubClient, error) {
	api, err := runnerURL("GITHUB_API_URL")
	if err != nil {
		return githubClient{}, err
	}

	token, err := requiredRunner("GITHUB_TOKEN")
	if err != nil {
		return githubClient{}, err
	}

	return githubClient{
		api:        api,
		token:      token,
		repository: repository,
		client: http.Client{
			Timeout: githubTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (client *githubClient) request(
	ctx context.Context,
	method string,
	endpoint string,
	body io.Reader,
	expected int,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		client.api+"/repos/"+client.repository+endpoint,
		body,
	)
	if err != nil {
		return nil, errAPIRequest
	}

	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-Github-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.client.Do(request)
	if err != nil {
		return nil, errAPITransport
	}

	if response.StatusCode != expected {
		_ = response.Body.Close()
		slog.Error(
			"retirement.github_request_failed",
			"method",
			method,
			"status",
			response.StatusCode,
			"err",
			errAPIStatus,
		)

		return nil, fmt.Errorf("%w: %s returned HTTP %d", errAPIStatus, method, response.StatusCode)
	}

	return response, nil
}

func (client *githubClient) confirm(ctx context.Context, event githubEvent) error {
	response, err := client.request(
		ctx,
		http.MethodGet,
		"/pulls/"+strconv.Itoa(event.Number),
		nil,
		http.StatusOK,
	)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	var current githubPull
	decoder := json.NewDecoder(io.LimitReader(response.Body, githubResponseLimit))
	if decoder.Decode(&current) != nil {
		return errAPIJSON
	}

	var trailing json.RawMessage
	if !errors.Is(decoder.Decode(&trailing), io.EOF) {
		return errAPITrailing
	}

	if err := current.validate(client.repository); err != nil {
		return err
	}

	if current.Number != event.Number || current.Base.SHA != event.PullRequest.Base.SHA ||
		current.Head.SHA != event.PullRequest.Head.SHA {
		return errStalePull
	}

	return nil
}
