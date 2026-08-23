// Package release stages a published mwan release for a deploy. The operator
// names the release tag on every deploy; nothing is pinned in the repository.
// The archives are downloaded and verified against their GitHub attestations
// by go-makefile's selfupdate verifier, the same code the release workflow
// runs after publishing, then the one binary inside each platform archive is
// extracted into a per-tag directory that the playbooks copy from. The
// wanconfig stack bundle is unpacked beside the binaries and its packages are
// checked against its manifest, so a play can install packages the release
// attested. The tag's commit is resolved as well, so a playbook can confirm
// the binary it installed reports the commit the tag points at.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// StackBundleAsset is the wanconfig stack bundle a release publishes beside
// the binaries: the Debian packages a gateway installs instead of compiling.
const StackBundleAsset = "wanconfig-stack_linux_amd64.tar.gz"

// stackManifestName is the bundle member that lists every package it carries.
const stackManifestName = "manifest.txt"

// stackMemberPrefix is where the bundle keeps its packages.
const stackMemberPrefix = "debs/"

// stackDirName is the directory under the stage the bundle unpacks into.
const stackDirName = "wanconfig-stack"

// maxStackMemberBytes bounds one extracted bundle member.
const maxStackMemberBytes int64 = 256 << 20

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
	// StackDir is where the wanconfig stack bundle is unpacked: the manifest
	// at its root and the packages under debs/.
	StackDir string
	// StackManifest is the absolute path of the unpacked bundle manifest.
	StackManifest string
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

	stackDir := filepath.Join(stageDir, stackDirName)
	manifestPath, err := s.extractStack(ctx, filepath.Join(archiveDir, StackBundleAsset), stackDir)
	if err != nil {
		return Staged{}, err
	}

	commit, err := s.resolveTagCommit(ctx)
	if err != nil {
		return Staged{}, err
	}
	return Staged{Tag: s.tag, Commit: commit, Dir: stageDir, Binaries: binaries, StackDir: stackDir, StackManifest: manifestPath}, nil
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

// extractStack unpacks the stack bundle into stackDir: the manifest at its
// root and each package under debs/ by base name, so a member path can never
// land anywhere else. Every unpacked package is then checked against the
// manifest. A release cut before the packaging build ships no bundle, which
// fails here so a play that needs packages never runs against such a tag.
func (s stager) extractStack(ctx context.Context, archivePath, stackDir string) (string, error) {
	archive, err := os.Open(archivePath)
	if errors.Is(err, fs.ErrNotExist) {
		err := fmt.Errorf("release %s ships no %s: the wanconfig stack packages exist only in releases cut after the packaging build", s.tag, StackBundleAsset)
		s.log.ErrorContext(ctx, "release: stack bundle missing", "tag", s.tag, "err", err)
		return "", err
	}
	if err != nil {
		s.log.WarnContext(ctx, "release: stack bundle open failed", "archive", archivePath, "err", err)
		return "", fmt.Errorf("open stack bundle: %w", err)
	}
	defer func() { _ = archive.Close() }()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		s.log.WarnContext(ctx, "release: stack bundle gzip open failed", "archive", archivePath, "err", err)
		return "", fmt.Errorf("read stack bundle: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	manifestPath, err := s.unpackStackMembers(ctx, tar.NewReader(gzipReader), archivePath, stackDir)
	if err != nil {
		return "", err
	}
	if err := s.verifyStackManifest(ctx, manifestPath, stackDir); err != nil {
		return "", err
	}
	return manifestPath, nil
}

// unpackStackMembers writes each bundle member and returns the manifest path.
func (s stager) unpackStackMembers(ctx context.Context, tarReader *tar.Reader, archivePath, stackDir string) (string, error) {
	manifestPath := ""
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.log.WarnContext(ctx, "release: stack bundle member read failed", "archive", archivePath, "err", err)
			return "", fmt.Errorf("read stack bundle member: %w", err)
		}
		destPath, err := s.stackMemberPath(ctx, header.Name, stackDir)
		if err != nil {
			return "", err
		}
		if err := s.writeStackMember(ctx, tarReader, header.Size, destPath); err != nil {
			return "", err
		}
		if header.Name == stackManifestName {
			manifestPath = destPath
		}
	}
	if manifestPath == "" {
		err := fmt.Errorf("stack bundle has no %s member", stackManifestName)
		s.log.WarnContext(ctx, "release: stack bundle missing manifest", "archive", archivePath, "err", err)
		return "", err
	}
	return manifestPath, nil
}

// stackMemberPath maps one bundle member name to its destination, rejecting
// everything but the manifest and debs/<name>.deb.
func (s stager) stackMemberPath(ctx context.Context, member, stackDir string) (string, error) {
	if member == stackManifestName {
		return filepath.Join(stackDir, stackManifestName), nil
	}
	base := strings.TrimPrefix(member, stackMemberPrefix)
	if member == base || base != filepath.Base(base) || !strings.HasSuffix(base, ".deb") || strings.Contains(member, "..") {
		err := fmt.Errorf("unexpected stack bundle member %q", member)
		s.log.WarnContext(ctx, "release: stack bundle member rejected", "member", member, "err", err)
		return "", err
	}
	// filepath.Base in the join keeps the destination inside the debs
	// directory whatever the member name held.
	return filepath.Join(stackDir, stackMemberPrefix, filepath.Base(base)), nil
}

// writeStackMember streams one member to destPath through a temporary file in
// the same directory, so a partial write never sits at the final path.
func (s stager) writeStackMember(ctx context.Context, reader io.Reader, size int64, destPath string) error {
	if size <= 0 || size > maxStackMemberBytes {
		err := fmt.Errorf("stack member size %d outside (0, %d]", size, maxStackMemberBytes)
		s.log.WarnContext(ctx, "release: stack member size rejected", "dest", destPath, "size", size, "err", err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		s.log.WarnContext(ctx, "release: stack dir create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create stack dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".*.partial")
	if err != nil {
		s.log.WarnContext(ctx, "release: temp stack member create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create temp stack member: %w", err)
	}
	tempPath := temp.Name()
	if _, err := io.Copy(temp, io.LimitReader(reader, size)); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: stack member write failed", "dest", destPath, "err", err)
		return fmt.Errorf("write stack member: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: stack member close failed", "dest", destPath, "err", err)
		return fmt.Errorf("close stack member: %w", err)
	}
	if err := os.Rename(tempPath, destPath); err != nil {
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: stack member place failed", "dest", destPath, "err", err)
		return fmt.Errorf("place stack member: %w", err)
	}
	return nil
}

// verifyStackManifest checks every manifest line against the unpacked
// packages: the named file exists, its sha256 matches, and no unpacked
// package is unlisted.
func (s stager) verifyStackManifest(ctx context.Context, manifestPath, stackDir string) error {
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		s.log.WarnContext(ctx, "release: stack manifest read failed", "path", manifestPath, "err", err)
		return fmt.Errorf("read stack manifest: %w", err)
	}
	listed := map[string]bool{}
	for line := range strings.Lines(string(content)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if err := s.verifyStackManifestLine(ctx, line, stackDir, listed); err != nil {
			return err
		}
	}
	debs, err := filepath.Glob(filepath.Join(stackDir, stackMemberPrefix, "*.deb"))
	if err != nil {
		s.log.WarnContext(ctx, "release: stack package list failed", "dir", stackDir, "err", err)
		return fmt.Errorf("list stack packages: %w", err)
	}
	if len(listed) == 0 || len(debs) != len(listed) {
		err := fmt.Errorf("stack bundle carries %d packages but its manifest lists %d", len(debs), len(listed))
		s.log.WarnContext(ctx, "release: stack manifest incomplete", "packages", len(debs), "listed", len(listed), "err", err)
		return err
	}
	s.log.InfoContext(ctx, "release: stack bundle staged", "dir", stackDir, "packages", len(debs))
	return nil
}

// verifyStackManifestLine checks one "package version architecture sha256
// file" line and records the file it lists.
func (s stager) verifyStackManifestLine(ctx context.Context, line, stackDir string, listed map[string]bool) error {
	fields := strings.Fields(line)
	if len(fields) != 5 {
		err := fmt.Errorf("stack manifest line %q is not \"package version architecture sha256 file\"", line)
		s.log.WarnContext(ctx, "release: stack manifest line rejected", "line", line, "err", err)
		return err
	}
	wantSum, file := fields[3], fields[4]
	memberPath, err := s.stackMemberPath(ctx, file, stackDir)
	if err != nil {
		return err
	}
	gotSum, err := s.stackFileSHA256(ctx, memberPath)
	if err != nil {
		return err
	}
	if gotSum != wantSum {
		err := fmt.Errorf("stack package %s sha256 %s does not match the manifest's %s", file, gotSum, wantSum)
		s.log.WarnContext(ctx, "release: stack package checksum mismatch", "file", file, "err", err)
		return err
	}
	listed[filepath.Base(file)] = true
	return nil
}

// stackFileSHA256 hashes one unpacked bundle file.
func (s stager) stackFileSHA256(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		s.log.WarnContext(ctx, "release: stack package open failed", "path", path, "err", err)
		return "", fmt.Errorf("open stack package: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		s.log.WarnContext(ctx, "release: stack package hash failed", "path", path, "err", err)
		return "", fmt.Errorf("hash stack package: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
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
