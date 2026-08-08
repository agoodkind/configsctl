// Package authorizedkeys builds SSH authorization bundles from GitHub keys.
package authorizedkeys

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	bundleMode       = 0o644
	githubKeysURL    = "https://github.com/%s.keys"
	maxResponseBytes = 1 << 20
	requestTimeout   = 30 * time.Second
)

var (
	// ErrHelp reports that the caller requested command help.
	ErrHelp           = errors.New("help requested")
	githubUserPattern = regexp.MustCompile(`\A[[:alnum:]](?:[[:alnum:]-]{0,37}[[:alnum:]])?\z`)
)

type options struct {
	githubUser string
	outPath    string
	extraKey   string
	extraOut   string
}

type pendingFile struct {
	target string
	temp   string
}

type loggedError struct {
	err error
}

func (e *loggedError) Error() string {
	return e.err.Error()
}

func (e *loggedError) Unwrap() error {
	return e.err
}

// IsLogged reports whether the error already produced a structured log event.
func IsLogged(err error) bool {
	var target *loggedError
	return errors.As(err, &target)
}

// Run parses the command arguments, fetches GitHub keys, and replaces each bundle.
func Run(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
	client *http.Client,
) error {
	slog.DebugContext(ctx, "deploy authorized keys started")
	parsed, err := parseOptions(ctx, arguments, stdout)
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}

	extraKeys, err := readExtraKey(ctx, parsed.extraKey)
	if err != nil {
		return err
	}
	humanKeys, err := readGitHubKeys(ctx, client, parsed.githubUser)
	if err != nil {
		return err
	}
	return writeBundles(ctx, parsed, humanKeys, extraKeys)
}

func parseOptions(
	ctx context.Context,
	arguments []string,
	stdout io.Writer,
) (options, error) {
	empty := options{githubUser: "", outPath: "", extraKey: "", extraOut: ""}
	parsed := empty
	flags := flag.NewFlagSet("deploy-authorized-keys", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&parsed.githubUser, "github-user", "", "GitHub username")
	flags.StringVar(&parsed.outPath, "out", "", "GitHub-only bundle path")
	flags.StringVar(&parsed.extraKey, "extra-pubkey", "", "additional public key path")
	flags.StringVar(&parsed.extraOut, "extra-out", "", "combined bundle path")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if usageErr := printUsage(ctx, stdout); usageErr != nil {
				return empty, usageErr
			}
			return empty, ErrHelp
		}
		return empty, errors.New(err.Error())
	}
	if flags.NArg() != 0 {
		return empty, fmt.Errorf("unknown arg: %s", flags.Arg(0))
	}
	if parsed.githubUser == "" {
		return empty, errors.New("--github-user is required")
	}
	if !githubUserPattern.MatchString(parsed.githubUser) {
		return empty, errors.New("--github-user is invalid")
	}
	if parsed.outPath == "" {
		return empty, errors.New("--out is required")
	}
	if parsed.extraKey != "" && parsed.extraOut == "" {
		return empty, errors.New("--extra-out is required with --extra-pubkey")
	}
	if parsed.extraKey == "" && parsed.extraOut != "" {
		return empty, errors.New("--extra-pubkey is required with --extra-out")
	}
	if parsed.extraOut != "" {
		equivalent, err := sameOutputPath(ctx, parsed.outPath, parsed.extraOut)
		if err != nil {
			return empty, err
		}
		if equivalent {
			return empty, errors.New("--out and --extra-out must differ")
		}
	}
	return parsed, nil
}

func printUsage(ctx context.Context, writer io.Writer) error {
	slog.DebugContext(ctx, "write deploy authorized keys usage")
	_, err := fmt.Fprintln(writer, `deploy-authorized-keys

Fetch GitHub SSH keys and write an authorized_keys bundle.

Usage:
  deploy-authorized-keys --github-user <user> --out <human-bundle> \
    [--extra-pubkey <path> --extra-out <combined-bundle>]

The human bundle contains GitHub keys only. The combined bundle adds the
optional public key.`)
	if err != nil {
		return logWrap(ctx, "write deploy authorized keys usage", err)
	}
	return nil
}

func sameOutputPath(ctx context.Context, first string, second string) (bool, error) {
	firstCanonical, err := readCanonicalOutputPath(ctx, first)
	if err != nil {
		return false, err
	}
	secondCanonical, err := readCanonicalOutputPath(ctx, second)
	if err != nil {
		return false, err
	}
	return firstCanonical == secondCanonical, nil
}

func readCanonicalOutputPath(ctx context.Context, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", logWrap(ctx, "resolve absolute output path", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", logWrap(ctx, "resolve output parent", err)
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func readExtraKey(ctx context.Context, path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, logWrap(ctx, "read extra public key", err)
	}
	return contents, nil
}

func readGitHubKeys(
	ctx context.Context,
	client *http.Client,
	githubUser string,
) ([]byte, error) {
	requestURL := fmt.Sprintf(githubKeysURL, githubUser)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, logWrap(ctx, "build GitHub SSH key request", err)
	}
	request.Header.Set("Accept", "text/plain")

	response, err := client.Do(request)
	if err != nil {
		return nil, logWrap(ctx, "fetch GitHub SSH keys", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, logWrap(ctx, "read GitHub SSH keys", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf(
			"GitHub SSH key response for %s exceeds %d bytes",
			githubUser,
			maxResponseBytes,
		)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message := strings.TrimSpace(string(body))
		if message == "" {
			return nil, fmt.Errorf(
				"GitHub SSH key fetch failed for %s: %s",
				githubUser,
				response.Status,
			)
		}
		return nil, fmt.Errorf(
			"GitHub SSH key fetch failed for %s: %s: %s",
			githubUser,
			response.Status,
			message,
		)
	}

	keys, err := normalizeKeys(body)
	if err != nil {
		return nil, fmt.Errorf("GitHub returned no SSH keys for %s", githubUser)
	}
	return keys, nil
}

func normalizeKeys(contents []byte) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(contents), "\r", ""), "\n")
	unique := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			unique[line] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return nil, errors.New("bundle contains no SSH keys")
	}

	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return []byte(strings.Join(keys, "\n") + "\n"), nil
}

func writeBundles(
	ctx context.Context,
	parsed options,
	humanKeys []byte,
	extraKeys []byte,
) error {
	humanFile, err := writePendingFile(ctx, parsed.outPath, humanKeys)
	if err != nil {
		return err
	}
	defer humanFile.cleanup()

	var combinedFile *pendingFile
	if parsed.extraOut != "" {
		combinedInput := make([]byte, 0, len(humanKeys)+len(extraKeys))
		combinedInput = append(combinedInput, humanKeys...)
		combinedInput = append(combinedInput, extraKeys...)
		combinedKeys, normalizeErr := normalizeKeys(combinedInput)
		if normalizeErr != nil {
			return logWrap(ctx, "build combined bundle", normalizeErr)
		}
		combinedFile, err = writePendingFile(ctx, parsed.extraOut, combinedKeys)
		if err != nil {
			return err
		}
		defer combinedFile.cleanup()
	}

	if err := humanFile.commit(ctx); err != nil {
		return err
	}
	if combinedFile != nil {
		if err := combinedFile.commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func writePendingFile(
	ctx context.Context,
	target string,
	contents []byte,
) (*pendingFile, error) {
	slog.DebugContext(ctx, "write pending authorized keys", "target", target)
	directory := filepath.Dir(target)
	temporary, err := os.CreateTemp(directory, ".deploy-authorized-keys-*")
	if err != nil {
		return nil, logWrap(ctx, "create temporary authorized keys file", err)
	}
	pending := &pendingFile{target: target, temp: temporary.Name()}
	if err := temporary.Chmod(bundleMode); err != nil {
		_ = temporary.Close()
		pending.cleanup()
		return nil, logWrap(ctx, "set authorized keys mode", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		pending.cleanup()
		return nil, logWrap(ctx, "write authorized keys", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		pending.cleanup()
		return nil, logWrap(ctx, "sync authorized keys", err)
	}
	if err := temporary.Close(); err != nil {
		pending.cleanup()
		return nil, logWrap(ctx, "close authorized keys", err)
	}
	return pending, nil
}

func (p *pendingFile) commit(ctx context.Context) error {
	slog.DebugContext(ctx, "commit authorized keys", "target", p.target)
	if err := os.Rename(p.temp, p.target); err != nil {
		return logWrap(ctx, "replace authorized keys", err)
	}
	p.temp = ""
	return nil
}

func (p *pendingFile) cleanup() {
	slog.Debug("cleanup pending authorized keys", "target", p.target)
	if p.temp != "" {
		_ = os.Remove(p.temp)
	}
}

func logWrap(ctx context.Context, message string, err error) error {
	slog.ErrorContext(ctx, message, "err", err)
	wrapped := fmt.Errorf("%s: %w", message, err)
	return &loggedError{err: wrapped}
}
