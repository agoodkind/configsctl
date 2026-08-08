package ansible

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"goodkind.io/configs/internal/authorizedkeys"
)

const (
	githubKeyA = "ssh-ed25519 AAAAGITHUBA github-a"
	githubKeyB = "ssh-rsa AAAAGITHUBB github-b"
	localKey   = "ssh-ed25519 AAAALOCAL local"
	agentKey   = "ssh-ed25519 AAAAAGENT agent"
	extraKey   = `from="2001:db8::1/128" ssh-ed25519 AAAASERVICE sshpiper-upstream`
)

type authorizedKeysHarness struct {
	path       string
	home       string
	fetchCount *atomic.Int32
}

type roundTripFunc func(*http.Request) (*http.Response, error)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write unavailable")
}

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newAuthorizedKeysHarness(t *testing.T) authorizedKeysHarness {
	t.Helper()

	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	fakeBin := t.TempDir()
	installExecutable(
		t,
		filepath.Join(workingDirectory, "testdata", "fake-ssh-add.sh"),
		filepath.Join(fakeBin, "ssh-add"),
	)

	home := t.TempDir()
	sshDirectory := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDirectory, 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", sshDirectory, err)
	}
	if err := os.WriteFile(
		filepath.Join(sshDirectory, "id_local.pub"),
		[]byte(localKey+"\n"),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile local key: %v", err)
	}

	return authorizedKeysHarness{
		path:       fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		home:       home,
		fetchCount: &atomic.Int32{},
	}
}

func installExecutable(t *testing.T, source string, destination string) {
	t.Helper()

	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", source, err)
	}
	if err := os.WriteFile(destination, contents, 0o700); err != nil {
		t.Fatalf("WriteFile %s: %v", destination, err)
	}
}

func (h authorizedKeysHarness) run(
	t *testing.T,
	githubBody string,
	githubError string,
	arguments ...string,
) ([]byte, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		h.fetchCount.Add(1)
		if githubError != "" {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = writer.Write([]byte(githubBody + githubError))
	}))
	t.Cleanup(server.Close)

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("Parse test server URL: %v", err)
	}
	client := server.Client()
	baseTransport := client.Transport
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		cloned := request.Clone(request.Context())
		cloned.URL.Scheme = target.Scheme
		cloned.URL.Host = target.Host
		return baseTransport.RoundTrip(cloned)
	})
	t.Setenv("FAKE_SSH_ADD_KEY", agentKey)
	t.Setenv("HOME", h.home)
	t.Setenv("PATH", h.path)

	var output bytes.Buffer
	runErr := authorizedkeys.Run(context.Background(), arguments, &output, client)
	if runErr != nil {
		_, _ = fmt.Fprintln(&output, runErr)
	}
	return output.Bytes(), runErr
}

func requireBundleLines(t *testing.T, path string, want []string) {
	t.Helper()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	got := strings.Split(strings.TrimSpace(string(contents)), "\n")
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("bundle lines = %q, want %q", got, want)
	}
}

func requireNoFile(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("output %s exists after failure", path)
	}
}

func requireFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %04o, want %04o", path, got, want)
	}
}

func TestDeployAuthorizedKeysWritesGitHubAndRestrictedBundles(t *testing.T) {
	harness := newAuthorizedKeysHarness(t)
	outputDirectory := t.TempDir()
	humanBundle := filepath.Join(outputDirectory, "human")
	combinedBundle := filepath.Join(outputDirectory, "combined")
	extraPublicKey := filepath.Join(t.TempDir(), "upstream.pub")
	if err := os.WriteFile(extraPublicKey, []byte(extraKey+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile extra key: %v", err)
	}

	githubBody := "\r\n" + githubKeyB + "\r\n" + githubKeyA + "\n" + githubKeyA + "\n\n"
	output, err := harness.run(
		t,
		githubBody,
		"",
		"--github-user",
		"agoodkind",
		"--out",
		humanBundle,
		"--extra-pubkey",
		extraPublicKey,
		"--extra-out",
		combinedBundle,
	)
	if err != nil {
		t.Fatalf("deploy-authorized-keys: %v\n%s", err, output)
	}

	requireBundleLines(t, humanBundle, []string{githubKeyA, githubKeyB})
	requireBundleLines(t, combinedBundle, []string{extraKey, githubKeyA, githubKeyB})
	requireFileMode(t, humanBundle, 0o644)
	requireFileMode(t, combinedBundle, 0o644)
	if got := harness.fetchCount.Load(); got != 1 {
		t.Fatalf("GitHub fetch count = %d, want 1", got)
	}
}

func TestDeployAuthorizedKeysRejectsFetchFailureWithoutOutputs(t *testing.T) {
	harness := newAuthorizedKeysHarness(t)
	outputDirectory := t.TempDir()
	humanBundle := filepath.Join(outputDirectory, "human")
	combinedBundle := filepath.Join(outputDirectory, "combined")
	extraPublicKey := filepath.Join(t.TempDir(), "upstream.pub")
	if err := os.WriteFile(extraPublicKey, []byte(extraKey+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile extra key: %v", err)
	}

	output, err := harness.run(
		t,
		"",
		"GitHub unavailable",
		"--github-user",
		"agoodkind",
		"--out",
		humanBundle,
		"--extra-pubkey",
		extraPublicKey,
		"--extra-out",
		combinedBundle,
	)
	if err == nil {
		t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
	}
	if !strings.Contains(string(output), "GitHub unavailable") {
		t.Fatalf("failure did not come from GitHub fetch:\n%s", output)
	}

	requireNoFile(t, humanBundle)
	requireNoFile(t, combinedBundle)
}

func TestDeployAuthorizedKeysReportsEmptyFetchFailureStatusOnce(t *testing.T) {
	harness := newAuthorizedKeysHarness(t)
	humanBundle := filepath.Join(t.TempDir(), "human")

	output, err := harness.run(
		t,
		"",
		" ",
		"--github-user",
		"agoodkind",
		"--out",
		humanBundle,
	)
	if err == nil {
		t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
	}
	if got := strings.Count(string(output), "503 Service Unavailable"); got != 1 {
		t.Fatalf("HTTP status count = %d, want 1:\n%s", got, output)
	}

	requireNoFile(t, humanBundle)
}

func TestDeployAuthorizedKeysRejectsEmptyGitHubKeysWithoutOutputs(t *testing.T) {
	harness := newAuthorizedKeysHarness(t)
	humanBundle := filepath.Join(t.TempDir(), "human")

	if output, err := harness.run(
		t,
		"\r\n\n",
		"",
		"--github-user",
		"agoodkind",
		"--out",
		humanBundle,
	); err == nil {
		t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
	}

	requireNoFile(t, humanBundle)
}

func TestDeployAuthorizedKeysRequiresArguments(t *testing.T) {
	testCases := []struct {
		name              string
		includeGitHubUser bool
		includeOutput     bool
		trailingOption    string
	}{
		{name: "GitHub user", includeOutput: true},
		{name: "output", includeGitHubUser: true},
		{name: "option value", trailingOption: "--github-user"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthorizedKeysHarness(t)
			outputDirectory := t.TempDir()
			humanBundle := filepath.Join(outputDirectory, "human")
			combinedBundle := filepath.Join(outputDirectory, "combined")
			arguments := make([]string, 0, 4)
			if testCase.includeGitHubUser {
				arguments = append(arguments, "--github-user", "agoodkind")
			}
			if testCase.includeOutput {
				arguments = append(arguments, "--out", humanBundle)
			}
			if testCase.trailingOption != "" {
				arguments = append(arguments, testCase.trailingOption)
			}
			if output, err := harness.run(t, githubKeyA+"\n", "", arguments...); err == nil {
				t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
			}
			requireNoFile(t, humanBundle)
			requireNoFile(t, combinedBundle)
		})
	}
}

func TestDeployAuthorizedKeysRequiresPairedExtraOutputs(t *testing.T) {
	testCases := []struct {
		name               string
		includeExtraPubkey bool
		includeExtraOut    bool
	}{
		{
			name:               "extra output",
			includeExtraPubkey: true,
		},
		{
			name:            "extra public key",
			includeExtraOut: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthorizedKeysHarness(t)
			outputDirectory := t.TempDir()
			humanBundle := filepath.Join(outputDirectory, "human")
			combinedBundle := filepath.Join(outputDirectory, "combined")
			extraPublicKey := filepath.Join(t.TempDir(), "upstream.pub")
			if err := os.WriteFile(extraPublicKey, []byte(extraKey+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile extra key: %v", err)
			}
			arguments := []string{"--github-user", "agoodkind", "--out", humanBundle}
			if testCase.includeExtraPubkey {
				arguments = append(arguments, "--extra-pubkey", extraPublicKey)
			}
			if testCase.includeExtraOut {
				arguments = append(arguments, "--extra-out", combinedBundle)
			}
			if output, err := harness.run(t, githubKeyA+"\n", "", arguments...); err == nil {
				t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
			}
			requireNoFile(t, humanBundle)
			requireNoFile(t, combinedBundle)
		})
	}
}

func TestDeployAuthorizedKeysRejectsEquivalentOutputPaths(t *testing.T) {
	testCases := []struct {
		name        string
		outputPaths func(*testing.T) (string, string)
	}{
		{
			name: "relative component",
			outputPaths: func(t *testing.T) (string, string) {
				t.Helper()
				directory := t.TempDir()
				return filepath.Join(directory, "keys"), directory + "/./keys"
			},
		},
		{
			name: "symlinked parent",
			outputPaths: func(t *testing.T) (string, string) {
				t.Helper()
				realDirectory := t.TempDir()
				linkDirectory := filepath.Join(t.TempDir(), "keys-dir")
				if err := os.Symlink(realDirectory, linkDirectory); err != nil {
					t.Fatalf("Symlink: %v", err)
				}
				return filepath.Join(realDirectory, "keys"), filepath.Join(linkDirectory, "keys")
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthorizedKeysHarness(t)
			humanBundle, combinedBundle := testCase.outputPaths(t)
			extraPublicKey := filepath.Join(t.TempDir(), "upstream.pub")
			if err := os.WriteFile(extraPublicKey, []byte(extraKey+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile extra key: %v", err)
			}

			output, err := harness.run(
				t,
				githubKeyA+"\n",
				"",
				"--github-user",
				"agoodkind",
				"--out",
				humanBundle,
				"--extra-pubkey",
				extraPublicKey,
				"--extra-out",
				combinedBundle,
			)
			if err == nil {
				t.Fatalf("deploy-authorized-keys succeeded, want failure\n%s", output)
			}
			if !strings.Contains(string(output), "--out and --extra-out must differ") {
				t.Fatalf("unexpected error:\n%s", output)
			}
			requireNoFile(t, humanBundle)
		})
	}
}

func TestDeployAuthorizedKeysReportsUsageWriteFailure(t *testing.T) {
	err := authorizedkeys.Run(
		context.Background(),
		[]string{"--help"},
		failingWriter{},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "write unavailable") {
		t.Fatalf("Run error = %v, want usage write failure", err)
	}
}
