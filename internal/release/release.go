// Package release stages a published mwan release for a deploy. The operator
// names the release tag on every deploy; nothing is pinned in the repository.
// The archives are downloaded and verified against their GitHub attestations
// by go-makefile's selfupdate verifier, the same code the release workflow
// runs after publishing, then the one binary inside each platform archive is
// extracted into a per-tag directory that the playbooks copy from. The tag's
// commit is resolved as well, so a playbook can confirm the binary it
// installed reports the commit the tag points at.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"goodkind.io/go-makefile/selfupdate"
)

// Repo is the GitHub repository the mwan releases publish to.
const Repo = "agoodkind/configs"

// Binary is the released binary name, which is also the archive prefix.
const Binary = "mwan"

// Platforms are the os_arch archives every release ships and every deploy
// stages, in the form the archive names carry.
var Platforms = []string{"linux_amd64", "freebsd_amd64"}

// maxBinaryBytes bounds one extracted binary. The static linux artifact is
// about 30 MB; a member past this limit is not the binary this package expects.
const maxBinaryBytes int64 = 256 << 20

// archiveMemberREADME is the only member besides the binary that a release
// archive may carry.
const archiveMemberREADME = "README.md"

// defaultAPIBaseURL is the GitHub API root the tag lookup uses.
const defaultAPIBaseURL = "https://api.github.com"

// defaultHTTPTimeout bounds one request when the caller supplies no client. It
// covers the archive downloads too, so it is sized for a 30 MB asset on a slow
// link rather than for an API call.
const defaultHTTPTimeout = 10 * time.Minute

// tagPattern is the character set a release tag may use. The tag becomes a
// path segment under the cache root and a path segment of the GitHub API URL,
// so anything that could escape either (a slash, a dot-dot, a query or
// fragment character) is refused before it reaches them. Release tags here are
// either <yyyymmddHHMM>-<n>-<sha7> or a v-prefixed version.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// gitObjectType is the type field of a git object the GitHub API returns.
type gitObjectType string

// gitObjectTypeTag is an annotated tag, whose commit is one dereference away.
const gitObjectTypeTag gitObjectType = "tag"

// Verifier downloads every archive of the tagged release into
// options.CacheDir and verifies each one. selfupdate.VerifyReleaseAssets is
// the production value; tests supply a local one.
type Verifier func(ctx context.Context, options selfupdate.Options, tag string) error

// FetchOptions names one release to stage.
type FetchOptions struct {
	// Tag is the release tag, required.
	Tag string
	// CacheRoot is the directory the per-tag stage directories live under.
	CacheRoot string
	// Token authenticates GitHub API calls. Empty means anonymous, which the
	// public repository allows.
	Token string
	// APIBaseURL overrides the GitHub API root, for tests. Empty means
	// https://api.github.com.
	APIBaseURL string
	// Client makes the commit lookup request. Nil means http.DefaultClient.
	Client *http.Client
	// Verify replaces the release verifier. Nil means
	// selfupdate.VerifyReleaseAssets.
	Verify Verifier
	// Log receives progress. Nil means slog.Default().
	Log *slog.Logger
}

// Staged describes a release that is verified and unpacked on the controller.
type Staged struct {
	// Tag is the release tag that was staged.
	Tag string
	// Commit is the full commit SHA the tag points at.
	Commit string
	// Dir is the per-tag stage directory. Each platform's binary sits at
	// Dir/<platform>/<Binary>.
	Dir string
	// Binaries maps each platform to the absolute path of its extracted binary.
	Binaries map[string]string
}

// stager carries the resolved options through one Fetch.
type stager struct {
	tag        string
	cacheRoot  string
	token      string
	apiBaseURL string
	client     *http.Client
	verify     Verifier
	log        *slog.Logger
}

// Fetch downloads, verifies, and unpacks the tagged release, returning where
// each platform's binary now sits.
func Fetch(ctx context.Context, opts FetchOptions) (Staged, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if strings.TrimSpace(opts.Tag) == "" {
		err := errors.New("release: tag is required")
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	if !tagPattern.MatchString(opts.Tag) || strings.Contains(opts.Tag, "..") {
		err := fmt.Errorf("release: tag %q may only use letters, digits, dot, underscore, and dash, without a dot-dot sequence", opts.Tag)
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	if strings.TrimSpace(opts.CacheRoot) == "" {
		err := errors.New("release: cache root is required")
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	s := stager{
		tag:        opts.Tag,
		cacheRoot:  opts.CacheRoot,
		token:      opts.Token,
		apiBaseURL: strings.TrimRight(opts.APIBaseURL, "/"),
		client:     opts.Client,
		verify:     opts.Verify,
		log:        log,
	}
	if s.apiBaseURL == "" {
		s.apiBaseURL = defaultAPIBaseURL
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if s.verify == nil {
		s.verify = selfupdate.VerifyReleaseAssets
	}
	return s.run(ctx)
}

func (s stager) run(ctx context.Context) (Staged, error) {
	stageDir, err := filepath.Abs(filepath.Join(s.cacheRoot, s.tag))
	if err != nil {
		s.log.ErrorContext(ctx, "release: stage dir resolve failed", "tag", s.tag, "err", err)
		return Staged{}, fmt.Errorf("release: resolve stage dir: %w", err)
	}
	archiveDir := filepath.Join(stageDir, "archives")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		s.log.ErrorContext(ctx, "release: stage dir create failed", "path", archiveDir, "err", err)
		return Staged{}, fmt.Errorf("release: create stage dir: %w", err)
	}

	s.log.InfoContext(ctx, "release: verify", "tag", s.tag, "repo", Repo)
	verifyOptions := selfupdate.Options{
		Config: selfupdate.Config{
			Repo:       Repo,
			Binary:     Binary,
			APIBaseURL: s.apiBaseURL,
			AuthToken:  s.token,
		},
		Client:   s.client,
		CacheDir: archiveDir,
		Log:      s.log,
	}
	if err := s.verify(ctx, verifyOptions, s.tag); err != nil {
		s.log.ErrorContext(ctx, "release: verify failed", "tag", s.tag, "err", err)
		return Staged{}, fmt.Errorf("release: verify %s: %w", s.tag, err)
	}

	binaries := make(map[string]string, len(Platforms))
	for _, platform := range Platforms {
		archivePath := filepath.Join(archiveDir, Binary+"_"+platform+".tar.gz")
		binaryPath := filepath.Join(stageDir, platform, Binary)
		if err := s.extractBinary(ctx, archivePath, binaryPath); err != nil {
			s.log.ErrorContext(ctx, "release: extract failed", "platform", platform, "archive", archivePath, "err", err)
			return Staged{}, fmt.Errorf("release: %s: %w", platform, err)
		}
		binaries[platform] = binaryPath
	}
	s.log.InfoContext(ctx, "release: binaries staged", "tag", s.tag, "dir", stageDir, "platforms", len(binaries))

	commit, err := s.resolveTagCommit(ctx)
	if err != nil {
		return Staged{}, err
	}
	return Staged{Tag: s.tag, Commit: commit, Dir: stageDir, Binaries: binaries}, nil
}

// extractBinary unpacks the single binary member of a release archive to
// destPath, atomically, with the executable bit set. Any member other than the
// binary and its README is rejected, so a tampered archive cannot place a file
// anywhere else.
func (s stager) extractBinary(ctx context.Context, archivePath, destPath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		s.log.WarnContext(ctx, "release: archive open failed", "archive", archivePath, "err", err)
		return fmt.Errorf("open archive: %w", err)
	}
	defer func() { _ = archive.Close() }()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		s.log.WarnContext(ctx, "release: archive gzip open failed", "archive", archivePath, "err", err)
		return fmt.Errorf("read archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()

	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.log.WarnContext(ctx, "release: archive member read failed", "archive", archivePath, "err", err)
			return fmt.Errorf("read archive member: %w", err)
		}
		switch header.Name {
		case Binary:
			if err := s.writeBinary(ctx, tarReader, header.Size, destPath); err != nil {
				return err
			}
			found = true
		case archiveMemberREADME:
			continue
		default:
			err := fmt.Errorf("unexpected archive member %q", header.Name)
			s.log.WarnContext(ctx, "release: archive member rejected", "archive", archivePath, "member", header.Name)
			return err
		}
	}
	if !found {
		err := fmt.Errorf("archive has no %s member", Binary)
		s.log.WarnContext(ctx, "release: archive missing binary", "archive", archivePath, "err", err)
		return err
	}
	return nil
}

// writeBinary streams one archive member to destPath through a temporary
// file in the same directory, so a partial write never sits at the final path.
func (s stager) writeBinary(ctx context.Context, reader io.Reader, size int64, destPath string) error {
	if size <= 0 || size > maxBinaryBytes {
		err := fmt.Errorf("binary size %d outside (0, %d]", size, maxBinaryBytes)
		s.log.WarnContext(ctx, "release: binary size rejected", "dest", destPath, "size", size)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		s.log.WarnContext(ctx, "release: platform dir create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create platform dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(destPath), Binary+".*.partial")
	if err != nil {
		s.log.WarnContext(ctx, "release: temp binary create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create temp binary: %w", err)
	}
	tempPath := temp.Name()
	if err := s.copyAndPlace(ctx, temp, reader, size, destPath); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	s.log.DebugContext(ctx, "release: binary written", "dest", destPath, "size", size)
	return nil
}

// copyAndPlace fills temp from reader, marks it executable, closes it, and
// renames it onto destPath. The caller removes temp on any error.
func (s stager) copyAndPlace(ctx context.Context, temp *os.File, reader io.Reader, size int64, destPath string) error {
	if _, err := io.Copy(temp, io.LimitReader(reader, size)); err != nil {
		_ = temp.Close()
		s.log.WarnContext(ctx, "release: binary write failed", "dest", destPath, "err", err)
		return fmt.Errorf("write binary: %w", err)
	}
	if err := temp.Chmod(0o755); err != nil {
		_ = temp.Close()
		s.log.WarnContext(ctx, "release: binary chmod failed", "dest", destPath, "err", err)
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := temp.Close(); err != nil {
		s.log.WarnContext(ctx, "release: binary close failed", "dest", destPath, "err", err)
		return fmt.Errorf("close binary: %w", err)
	}
	if err := os.Rename(temp.Name(), destPath); err != nil {
		s.log.WarnContext(ctx, "release: binary place failed", "dest", destPath, "err", err)
		return fmt.Errorf("place binary: %w", err)
	}
	return nil
}

type gitObject struct {
	SHA  string        `json:"sha"`
	Type gitObjectType `json:"type"`
}

type gitRefResponse struct {
	Object gitObject `json:"object"`
}

// resolveTagCommit returns the full commit SHA the release tag points at,
// dereferencing an annotated tag once.
func (s stager) resolveTagCommit(ctx context.Context) (string, error) {
	ref, err := s.getGitObject(ctx, s.apiBaseURL+"/repos/"+Repo+"/git/ref/tags/"+s.tag)
	if err != nil {
		s.log.ErrorContext(ctx, "release: tag lookup failed", "tag", s.tag, "err", err)
		return "", fmt.Errorf("release: resolve tag %s: %w", s.tag, err)
	}
	if ref.Object.Type != gitObjectTypeTag {
		return ref.Object.SHA, nil
	}
	annotated, err := s.getGitObject(ctx, s.apiBaseURL+"/repos/"+Repo+"/git/tags/"+ref.Object.SHA)
	if err != nil {
		s.log.ErrorContext(ctx, "release: annotated tag dereference failed", "tag", s.tag, "err", err)
		return "", fmt.Errorf("release: dereference tag %s: %w", s.tag, err)
	}
	return annotated.Object.SHA, nil
}

// getGitObject fetches one git ref or tag object from the GitHub API. It is a
// process boundary, so it logs the request and wraps every error.
func (s stager) getGitObject(ctx context.Context, url string) (gitRefResponse, error) {
	s.log.DebugContext(ctx, "release: github api get", "url", url)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		s.log.WarnContext(ctx, "release: github api request build failed", "url", url, "err", err)
		return gitRefResponse{}, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if s.token != "" {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}
	response, err := s.client.Do(request)
	if err != nil {
		s.log.WarnContext(ctx, "release: github api request failed", "url", url, "err", err)
		return gitRefResponse{}, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		err := fmt.Errorf("GET %s: HTTP %d", url, response.StatusCode)
		s.log.WarnContext(ctx, "release: github api status", "url", url, "status", response.StatusCode)
		return gitRefResponse{}, err
	}
	var decoded gitRefResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		s.log.WarnContext(ctx, "release: github api decode failed", "url", url, "err", err)
		return gitRefResponse{}, fmt.Errorf("decode %s: %w", url, err)
	}
	return decoded, nil
}
