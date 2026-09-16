// Package release stages a published release of one source for a deploy. A
// source is the GitHub repository a release publishes to, the binary its
// archives carry, and the platforms a deploy stages. The operator names the
// release tag on every deploy; nothing is pinned in the repository. The
// archives are downloaded and verified against their GitHub attestations for
// the source's repository by go-makefile's selfupdate verifier, the same code
// the release workflow runs after publishing, then the one binary inside each
// platform archive is extracted into a per-source, per-tag directory that the
// playbooks copy from. A release that publishes a manifest lists the extra
// archives it carries; each one is unpacked beside the binaries and handed to
// the play as the variable the manifest names, so a new data file needs no
// change here. A gateway release without a manifest still stages the wanconfig
// stack bundle and checks its packages against the bundle's own listing. The
// tag's commit is resolved as well, so a playbook can confirm the binary it
// installed reports the commit the tag points at.
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
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"goodkind.io/go-makefile/selfupdate"
)

// Source describes one GitHub repository whose releases a deploy stages.
type Source struct {
	// Name is the directory under the cache root this source's tags stage
	// into, so two sources never share a stage directory.
	Name string
	// Repo is the GitHub repository the releases publish to, as owner/name.
	// The attestation verifier requires each archive to come from it.
	Repo string
	// Binary is the released binary name, which is also the archive prefix and
	// the only archive member besides the README.
	Binary string
	// Platforms are the os_arch archives a deploy stages, in the form the
	// archive names carry.
	Platforms []string
	// StackBundle reports whether the release publishes the wanconfig stack
	// bundle beside the binaries, which a stage then requires and unpacks. It
	// is consulted only for a release without a manifest; a manifest is the
	// only source of extra assets when the release carries one.
	StackBundle bool
}

// Gateway is the mwan gateway release, published from agoodkind/configs.
var Gateway = Source{
	Name:        "mwan",
	Repo:        "agoodkind/configs",
	Binary:      "mwan",
	Platforms:   []string{"linux_amd64"},
	StackBundle: true,
}

// Opnsensectl is the opnsensectl release, published from
// agoodkind/opnsensectl for linux and FreeBSD.
var Opnsensectl = Source{
	Name:        "opnsensectl",
	Repo:        "agoodkind/opnsensectl",
	Binary:      "opnsensectl",
	Platforms:   []string{"linux_amd64", "freebsd_amd64"},
	StackBundle: false,
}

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

// maxMemberBytes bounds one extracted bundle or manifest asset member.
const maxMemberBytes int64 = 256 << 20

// ManifestAsset is the release manifest: an archive holding one member,
// release-manifest.json, that lists the extra assets the release carries. It
// is an archive rather than a bare JSON file because the release engine
// publishes, attests, and the verifier here downloads only .tar.gz assets, so
// the manifest is verified before it is read, exactly like every other asset.
// The platform suffix follows how the release engine names every archive it
// produces. This is the one asset name a stage knows; every other extra asset
// is named only by the manifest.
const ManifestAsset = "release-manifest_linux_amd64.tar.gz"

// manifestMemberName is the only member the manifest archive may carry.
const manifestMemberName = "release-manifest.json"

// maxManifestBytes bounds the manifest member. A listing of a few assets is
// well under a kilobyte.
const maxManifestBytes int64 = 1 << 20

// archiveSuffix is the asset name suffix the verifier downloads and attests.
// A manifest entry naming anything else was never verified, so it is refused
// before the stage looks for it.
const archiveSuffix = ".tar.gz"

// varPattern is the shape of a deploy variable a manifest entry may name: an
// Ansible variable name.
var varPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// memberSegmentPattern is one path segment of a manifest asset member. It is
// wider than tagPattern because the files a release stages carry the
// characters Debian package and YANG model names use, such as the @ between a
// module name and its revision, and it still refuses a leading dot or dash,
// so neither a hidden file nor a dot-dot segment passes.
var memberSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@+~-]*$`)

// manifestEntry is one extra asset a release manifest lists.
type manifestEntry struct {
	// Name is the release asset, an archive beside the binaries.
	Name string `json:"name"`
	// Unpack is the directory under the stage the archive unpacks into.
	Unpack string `json:"unpack"`
	// Var is the deploy variable that receives the unpacked directory.
	Var string `json:"var"`
}

// manifest is the content of release-manifest.json.
type manifest struct {
	Assets []manifestEntry `json:"assets"`
}

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
// either <yyyymmddHHMM>-<n>-<sha7> or a v-prefixed version. A source's name,
// binary, and platforms become path segments too and pass the same check.
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// isPathSegment reports whether value is safe as one path segment under the
// cache root and one segment of a GitHub API URL.
func isPathSegment(value string) bool {
	return tagPattern.MatchString(value) && !strings.Contains(value, "..")
}

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
	// Source is the repository, binary, and platforms to stage, required.
	Source Source
	// Tag is the release tag, required.
	Tag string
	// CacheRoot is the directory the per-source stage directories live under.
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
	// Dir is the stage directory, <CacheRoot>/<Source.Name>/<Tag>. Each
	// platform's binary sits at Dir/<platform>/<Source.Binary>.
	Dir string
	// Binaries maps each platform to the absolute path of its extracted binary.
	Binaries map[string]string
	// StackDir is where the wanconfig stack bundle is unpacked: the manifest
	// at its root and the packages under debs/. It is empty for a source
	// without a stack bundle.
	StackDir string
	// StackManifest is the absolute path of the unpacked bundle manifest, or
	// empty for a source without a stack bundle.
	StackManifest string
	// Assets maps each deploy variable the release manifest names to the
	// absolute directory its archive unpacked into. It is empty for a release
	// without a manifest.
	Assets map[string]string
}

// stager carries the resolved options through one Fetch.
type stager struct {
	source     Source
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
	if !isPathSegment(opts.Tag) {
		err := fmt.Errorf("release: tag %q may only use letters, digits, dot, underscore, and dash, without a dot-dot sequence", opts.Tag)
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	if strings.TrimSpace(opts.CacheRoot) == "" {
		err := errors.New("release: cache root is required")
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	if err := validateSource(opts.Source); err != nil {
		log.ErrorContext(ctx, "release: fetch refused", "err", err)
		return Staged{}, err
	}
	s := stager{
		source:     opts.Source,
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

// validateSource refuses a descriptor that names no repository, binary, or
// platform, or whose name, binary, or platforms could escape the stage
// directory.
func validateSource(source Source) error {
	if strings.TrimSpace(source.Repo) == "" || len(source.Platforms) == 0 {
		return fmt.Errorf("release: source %q needs a repo and at least one platform", source.Name)
	}
	segments := append([]string{source.Name, source.Binary}, source.Platforms...)
	for _, segment := range segments {
		if !isPathSegment(segment) {
			return fmt.Errorf("release: source %q has a name, binary, or platform %q that is not a plain path segment", source.Name, segment)
		}
	}
	return nil
}

func (s stager) run(ctx context.Context) (Staged, error) {
	stageDir, err := filepath.Abs(filepath.Join(s.cacheRoot, s.source.Name, s.tag))
	if err != nil {
		s.log.ErrorContext(ctx, "release: stage dir resolve failed", "source", s.source.Name, "tag", s.tag, "err", err)
		return Staged{}, fmt.Errorf("release: resolve stage dir: %w", err)
	}
	// The archive directory starts empty so every archive read below is one
	// the verifier wrote this run. An archive left by an earlier stage of the
	// tag, such as a manifest the release no longer publishes, would otherwise
	// sit beside the verified ones and be read as if it were verified.
	archiveDir := filepath.Join(stageDir, "archives")
	if err := os.RemoveAll(archiveDir); err != nil {
		s.log.ErrorContext(ctx, "release: stale archive dir remove failed", "path", archiveDir, "err", err)
		return Staged{}, fmt.Errorf("clear stage archives: %w", err)
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		s.log.ErrorContext(ctx, "release: stage dir create failed", "path", archiveDir, "err", err)
		return Staged{}, fmt.Errorf("release: create stage dir: %w", err)
	}

	s.log.InfoContext(ctx, "release: verify", "tag", s.tag, "repo", s.source.Repo)
	verifyOptions := selfupdate.Options{
		Config: selfupdate.Config{
			Repo:       s.source.Repo,
			Binary:     s.source.Binary,
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

	binaries := make(map[string]string, len(s.source.Platforms))
	for _, platform := range s.source.Platforms {
		archivePath := filepath.Join(archiveDir, s.source.Binary+"_"+platform+".tar.gz")
		binaryPath := filepath.Join(stageDir, platform, s.source.Binary)
		if err := s.extractBinary(ctx, archivePath, binaryPath); err != nil {
			s.log.ErrorContext(ctx, "release: extract failed", "platform", platform, "archive", archivePath, "err", err)
			return Staged{}, fmt.Errorf("release: %s: %w", platform, err)
		}
		binaries[platform] = binaryPath
	}
	s.log.InfoContext(ctx, "release: binaries staged", "source", s.source.Name, "tag", s.tag, "dir", stageDir, "platforms", len(binaries))

	extras, err := s.stageExtras(ctx, stageDir, archiveDir)
	if err != nil {
		return Staged{}, err
	}

	commit, err := s.resolveTagCommit(ctx)
	if err != nil {
		return Staged{}, err
	}
	return Staged{
		Tag:           s.tag,
		Commit:        commit,
		Dir:           stageDir,
		Binaries:      binaries,
		StackDir:      extras.stackDir,
		StackManifest: extras.stackManifest,
		Assets:        extras.assets,
	}, nil
}

// stagedExtras is what a release publishes beside its binaries: the assets
// its manifest lists, or the stack bundle of a source that declares one.
type stagedExtras struct {
	stackDir      string
	stackManifest string
	assets        map[string]string
}

// stageExtras unpacks whatever the release publishes beside its binaries. A
// manifest, when the verifier wrote one, is the only source of extra assets;
// the source's StackBundle flag is consulted only for a release without one.
func (s stager) stageExtras(ctx context.Context, stageDir, archiveDir string) (stagedExtras, error) {
	manifestArchive := filepath.Join(archiveDir, ManifestAsset)
	_, err := os.Stat(manifestArchive)
	if err == nil {
		assets, err := s.stageManifest(ctx, manifestArchive, stageDir, archiveDir)
		if err != nil {
			return stagedExtras{}, err
		}
		return stagedExtras{stackDir: "", stackManifest: "", assets: assets}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		s.log.ErrorContext(ctx, "release: manifest stat failed", "archive", manifestArchive, "err", err)
		return stagedExtras{}, fmt.Errorf("release: stat %s: %w", ManifestAsset, err)
	}
	if !s.source.StackBundle {
		return stagedExtras{stackDir: "", stackManifest: "", assets: nil}, nil
	}
	stackDir := filepath.Join(stageDir, stackDirName)
	manifestPath, err := s.extractStack(ctx, filepath.Join(archiveDir, StackBundleAsset), stackDir)
	if err != nil {
		return stagedExtras{}, err
	}
	return stagedExtras{stackDir: stackDir, stackManifest: manifestPath, assets: nil}, nil
}

// stageManifest reads the verified manifest and unpacks every asset it lists
// under the stage, returning the deploy variable each one is handed as.
func (s stager) stageManifest(ctx context.Context, manifestArchive, stageDir, archiveDir string) (map[string]string, error) {
	entries, err := s.readManifest(ctx, manifestArchive)
	if err != nil {
		return nil, err
	}
	assets := make(map[string]string, len(entries))
	for _, entry := range entries {
		destDir := filepath.Join(stageDir, entry.Unpack)
		if err := s.extractAsset(ctx, entry, filepath.Join(archiveDir, entry.Name), destDir); err != nil {
			return nil, err
		}
		assets[entry.Var] = destDir
	}
	s.log.InfoContext(ctx, "release: manifest assets staged", "tag", s.tag, "assets", len(assets))
	return assets, nil
}

// readManifest decodes the manifest archive's one member and checks every
// entry before any asset is touched. The archive is read only after the
// verifier accepted it, so an unverified manifest is never parsed.
func (s stager) readManifest(ctx context.Context, manifestArchive string) ([]manifestEntry, error) {
	content, err := s.readManifestMember(ctx, manifestArchive)
	if err != nil {
		return nil, err
	}
	var decoded manifest
	if err := json.Unmarshal(content, &decoded); err != nil {
		s.log.ErrorContext(ctx, "release: manifest decode failed", "archive", manifestArchive, "err", err)
		return nil, fmt.Errorf("release: decode %s: %w", manifestMemberName, err)
	}
	if err := s.validateManifest(ctx, decoded.Assets); err != nil {
		return nil, err
	}
	return decoded.Assets, nil
}

// readManifestMember returns the bytes of the manifest archive's only member,
// refusing an archive that carries anything but release-manifest.json.
func (s stager) readManifestMember(ctx context.Context, manifestArchive string) ([]byte, error) {
	archive, err := os.Open(manifestArchive)
	if err != nil {
		s.log.ErrorContext(ctx, "release: manifest open failed", "archive", manifestArchive, "err", err)
		return nil, fmt.Errorf("release: open %s: %w", ManifestAsset, err)
	}
	defer func() { _ = archive.Close() }()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		s.log.ErrorContext(ctx, "release: manifest gzip open failed", "archive", manifestArchive, "err", err)
		return nil, fmt.Errorf("release: read %s: %w", ManifestAsset, err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	var content []byte
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.log.ErrorContext(ctx, "release: manifest member read failed", "archive", manifestArchive, "err", err)
			return nil, fmt.Errorf("release: read %s member: %w", ManifestAsset, err)
		}
		if header.Name != manifestMemberName || content != nil {
			err := fmt.Errorf("release: %s must hold exactly one member, %s, but carries %q", ManifestAsset, manifestMemberName, header.Name)
			s.log.ErrorContext(ctx, "release: manifest member rejected", "member", header.Name, "err", err)
			return nil, err
		}
		if header.Size <= 0 || header.Size > maxManifestBytes {
			err := fmt.Errorf("release: %s size %d outside (0, %d]", manifestMemberName, header.Size, maxManifestBytes)
			s.log.ErrorContext(ctx, "release: manifest size rejected", "size", header.Size, "err", err)
			return nil, err
		}
		content, err = io.ReadAll(io.LimitReader(tarReader, header.Size))
		if err != nil {
			s.log.ErrorContext(ctx, "release: manifest member read failed", "archive", manifestArchive, "err", err)
			return nil, fmt.Errorf("release: read %s: %w", manifestMemberName, err)
		}
	}
	if content == nil {
		err := fmt.Errorf("release: %s has no %s member", ManifestAsset, manifestMemberName)
		s.log.ErrorContext(ctx, "release: manifest member missing", "archive", manifestArchive, "err", err)
		return nil, err
	}
	return content, nil
}

// validateManifest refuses an entry the stage could not honor: an asset the
// verifier never downloads, a name, directory, or variable that collides with
// what the binaries use, or a value that could escape the stage. Two entries
// sharing a name, an unpack directory, or a variable are refused as well, so
// one asset can never overwrite another.
func (s stager) validateManifest(ctx context.Context, entries []manifestEntry) error {
	reserved := map[string]bool{"archives": true}
	names := map[string]bool{ManifestAsset: true}
	for _, platform := range s.source.Platforms {
		reserved[platform] = true
		names[s.source.Binary+"_"+platform+archiveSuffix] = true
	}
	unpacks := map[string]bool{}
	vars := map[string]bool{}
	for _, entry := range entries {
		if err := validateManifestEntry(entry, names, reserved, unpacks, vars); err != nil {
			s.log.ErrorContext(ctx, "release: manifest entry rejected", "tag", s.tag, "asset", entry.Name, "err", err)
			return err
		}
		names[entry.Name] = true
		unpacks[entry.Unpack] = true
		vars[entry.Var] = true
	}
	return nil
}

// validateManifestEntry checks one entry against the names, directories, and
// variables already taken.
func validateManifestEntry(entry manifestEntry, names, reserved, unpacks, vars map[string]bool) error {
	if !isPathSegment(entry.Name) || !strings.HasSuffix(entry.Name, archiveSuffix) {
		return fmt.Errorf("release manifest asset %q is not a plain %s file name", entry.Name, archiveSuffix)
	}
	if names[entry.Name] {
		return fmt.Errorf("release manifest lists asset %q twice, or it is an archive the stage already uses", entry.Name)
	}
	if !isPathSegment(entry.Unpack) {
		return fmt.Errorf("release manifest asset %q has unpack %q, which is not a plain path segment", entry.Name, entry.Unpack)
	}
	if reserved[entry.Unpack] || unpacks[entry.Unpack] {
		return fmt.Errorf("release manifest asset %q has unpack %q, which another asset or the binaries already use", entry.Name, entry.Unpack)
	}
	if !varPattern.MatchString(entry.Var) {
		return fmt.Errorf("release manifest asset %q has var %q, which is not a variable name", entry.Name, entry.Var)
	}
	if vars[entry.Var] {
		return fmt.Errorf("release manifest asset %q has var %q, which another asset already uses", entry.Name, entry.Var)
	}
	return nil
}

// extractAsset unpacks one manifest-listed archive under destDir. Every member
// must be a regular file or directory at a plain relative path, so a member
// can never land outside destDir, and the archive must carry at least one
// file. A listed asset the release does not contain fails here by name.
func (s stager) extractAsset(ctx context.Context, entry manifestEntry, archivePath, destDir string) error {
	archive, err := os.Open(archivePath)
	if errors.Is(err, fs.ErrNotExist) {
		err := fmt.Errorf("release %s does not contain %s, which its manifest lists", s.tag, entry.Name)
		s.log.ErrorContext(ctx, "release: manifest asset missing", "tag", s.tag, "asset", entry.Name, "err", err)
		return err
	}
	if err != nil {
		s.log.WarnContext(ctx, "release: asset open failed", "archive", archivePath, "err", err)
		return fmt.Errorf("open asset %s: %w", entry.Name, err)
	}
	defer func() { _ = archive.Close() }()
	gzipReader, err := gzip.NewReader(archive)
	if err != nil {
		s.log.WarnContext(ctx, "release: asset gzip open failed", "archive", archivePath, "err", err)
		return fmt.Errorf("read asset %s: %w", entry.Name, err)
	}
	defer func() { _ = gzipReader.Close() }()
	files, err := s.unpackAssetMembers(ctx, tar.NewReader(gzipReader), entry, destDir)
	if err != nil {
		return err
	}
	if files == 0 {
		err := fmt.Errorf("asset %s has no file members", entry.Name)
		s.log.WarnContext(ctx, "release: asset empty", "asset", entry.Name, "err", err)
		return err
	}
	s.log.InfoContext(ctx, "release: asset staged", "asset", entry.Name, "dir", destDir, "files", files)
	return nil
}

// unpackAssetMembers writes each file member under destDir and returns how
// many it wrote. Directory members are created as their files are written.
func (s stager) unpackAssetMembers(ctx context.Context, tarReader *tar.Reader, entry manifestEntry, destDir string) (int, error) {
	files := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			s.log.WarnContext(ctx, "release: asset member read failed", "asset", entry.Name, "err", err)
			return 0, fmt.Errorf("read asset %s member: %w", entry.Name, err)
		}
		destPath, err := s.assetMemberPath(ctx, entry, header.Name, destDir)
		if err != nil {
			return 0, err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if err := s.writeMember(ctx, tarReader, header.Size, destPath); err != nil {
				return 0, err
			}
			files++
		default:
			err := fmt.Errorf("asset %s member %q is neither a file nor a directory", entry.Name, header.Name)
			s.log.WarnContext(ctx, "release: asset member rejected", "asset", entry.Name, "member", header.Name, "err", err)
			return 0, err
		}
	}
}

// assetMemberPath maps one member name to its destination under destDir,
// accepting only a relative path whose every segment is plain, so neither a
// leading slash nor a dot-dot can reach outside the asset's directory.
func (s stager) assetMemberPath(ctx context.Context, entry manifestEntry, member, destDir string) (string, error) {
	clean := path.Clean(member)
	segments := strings.Split(clean, "/")
	valid := clean != "." && !path.IsAbs(clean)
	for _, segment := range segments {
		if !memberSegmentPattern.MatchString(segment) {
			valid = false
		}
	}
	if !valid {
		err := fmt.Errorf("asset %s member %q is not a plain relative path", entry.Name, member)
		s.log.WarnContext(ctx, "release: asset member rejected", "asset", entry.Name, "member", member, "err", err)
		return "", err
	}
	return filepath.Join(destDir, filepath.FromSlash(clean)), nil
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
		case s.source.Binary:
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
		err := fmt.Errorf("archive has no %s member", s.source.Binary)
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
	temp, err := os.CreateTemp(filepath.Dir(destPath), s.source.Binary+".*.partial")
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
		if err := s.writeMember(ctx, tarReader, header.Size, destPath); err != nil {
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

// writeMember streams one bundle or asset member to destPath through a
// temporary file in the same directory, so a partial write never sits at the
// final path.
func (s stager) writeMember(ctx context.Context, reader io.Reader, size int64, destPath string) error {
	if size <= 0 || size > maxMemberBytes {
		err := fmt.Errorf("member size %d outside (0, %d]", size, maxMemberBytes)
		s.log.WarnContext(ctx, "release: member size rejected", "dest", destPath, "size", size, "err", err)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		s.log.WarnContext(ctx, "release: member dir create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create member dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".*.partial")
	if err != nil {
		s.log.WarnContext(ctx, "release: temp member create failed", "dest", destPath, "err", err)
		return fmt.Errorf("create temp member: %w", err)
	}
	tempPath := temp.Name()
	if _, err := io.Copy(temp, io.LimitReader(reader, size)); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: member write failed", "dest", destPath, "err", err)
		return fmt.Errorf("write member: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: member close failed", "dest", destPath, "err", err)
		return fmt.Errorf("close member: %w", err)
	}
	if err := os.Rename(tempPath, destPath); err != nil {
		_ = os.Remove(tempPath)
		s.log.WarnContext(ctx, "release: member place failed", "dest", destPath, "err", err)
		return fmt.Errorf("place member: %w", err)
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
	ref, err := s.getGitObject(ctx, s.apiBaseURL+"/repos/"+s.source.Repo+"/git/ref/tags/"+s.tag)
	if err != nil {
		s.log.ErrorContext(ctx, "release: tag lookup failed", "tag", s.tag, "err", err)
		return "", fmt.Errorf("release: resolve tag %s: %w", s.tag, err)
	}
	if ref.Object.Type != gitObjectTypeTag {
		return ref.Object.SHA, nil
	}
	annotated, err := s.getGitObject(ctx, s.apiBaseURL+"/repos/"+s.source.Repo+"/git/tags/"+ref.Object.SHA)
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
