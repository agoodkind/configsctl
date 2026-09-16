package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/go-makefile/selfupdate"
)

// tarGz builds a gzip-compressed tar with the given members, in order.
func tarGz(t *testing.T, members map[string][]byte, order []string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range order {
		content := members[name]
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// stackBundle builds a wanconfig stack bundle holding the named package
// contents, with a correct manifest line for each.
func stackBundle(t *testing.T, names []string, contents map[string][]byte) []byte {
	t.Helper()
	manifest := "# package version architecture sha256 file\n"
	members := map[string][]byte{}
	order := []string{stackManifestName}
	for _, name := range names {
		content := contents[name]
		sum := sha256.Sum256(content)
		manifest += strings.TrimSuffix(name, ".deb") + " 1.0 amd64 " + hex.EncodeToString(sum[:]) + " debs/" + name + "\n"
		members["debs/"+name] = content
		order = append(order, "debs/"+name)
	}
	members[stackManifestName] = []byte(manifest)
	return tarGz(t, members, order)
}

// testStackDebs are the packages the happy-path bundle carries.
var testStackDebs = map[string][]byte{
	"libyang3_3.13.6-1_amd64.deb":             []byte("libyang deb"),
	"mwan-wanconfig-rousette_2.0.0_amd64.deb": []byte("rousette deb"),
}

func testStackBundle(t *testing.T) []byte {
	t.Helper()
	return stackBundle(t, []string{"libyang3_3.13.6-1_amd64.deb", "mwan-wanconfig-rousette_2.0.0_amd64.deb"}, testStackDebs)
}

// binaryArchive builds a platform archive the way go-mk publishes one: the
// source's binary and a README.
func binaryArchive(t *testing.T, source Source, content string) []byte {
	t.Helper()
	return tarGz(t, map[string][]byte{source.Binary: []byte(content), "README.md": []byte("readme")}, []string{"README.md", source.Binary})
}

// archiveName is the release asset name of one platform archive.
func archiveName(source Source, platform string) string {
	return source.Binary + "_" + platform + ".tar.gz"
}

// completeRelease is every asset a well-formed release of source publishes:
// one archive per platform whose binary holds "<platform>-binary", plus the
// stack bundle when the source ships one.
func completeRelease(t *testing.T, source Source) map[string][]byte {
	t.Helper()
	assets := map[string][]byte{}
	for _, platform := range source.Platforms {
		assets[archiveName(source, platform)] = binaryArchive(t, source, platform+"-binary")
	}
	if source.StackBundle {
		assets[StackBundleAsset] = testStackBundle(t)
	}
	return assets
}

// writingVerifier stands in for the network verifier: it writes the given
// archives into the cache directory the way selfupdate does after verifying,
// and records the tag and directory it was asked about. It fails the test when
// the verifier is not asked about the source's repository and binary, which is
// what the attestation check is keyed on.
func writingVerifier(t *testing.T, source Source, archives map[string][]byte, seenTag *string, seenDir *string) Verifier {
	t.Helper()
	return func(_ context.Context, options selfupdate.Options, tag string) error {
		*seenTag = tag
		*seenDir = options.CacheDir
		if options.Config.Repo != source.Repo || options.Config.Binary != source.Binary {
			t.Fatalf("verifier got repo=%q binary=%q, want repo=%q binary=%q", options.Config.Repo, options.Config.Binary, source.Repo, source.Binary)
		}
		for name, content := range archives {
			if err := os.WriteFile(filepath.Join(options.CacheDir, name), content, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
}

// tagAPI serves the two git ref lookups Fetch makes for a lightweight and an
// annotated tag, under the source's repository only.
func tagAPI(t *testing.T, source Source) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+source.Repo+"/git/ref/tags/light", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "0123456789abcdef0123456789abcdef01234567", Type: gitObjectType("commit")}})
	})
	mux.HandleFunc("/repos/"+source.Repo+"/git/ref/tags/annotated", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "tagobject", Type: gitObjectTypeTag}})
	})
	mux.HandleFunc("/repos/"+source.Repo+"/git/tags/tagobject", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "fedcba9876543210fedcba9876543210fedcba98", Type: gitObjectType("commit")}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// fetchSources are the descriptors every source-generic test runs against.
var fetchSources = []Source{Gateway, Opnsensectl}

// TestFetchStagesEveryPlatformBinary pins the stage layout for each source:
// <root>/<source>/<tag>/<platform>/<binary>, one executable per platform the
// source names, verified against the source's repository.
func TestFetchStagesEveryPlatformBinary(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		t.Run(source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, source)
			var seenTag, seenDir string
			verify := writingVerifier(t, source, completeRelease(t, source), &seenTag, &seenDir)
			root := t.TempDir()

			staged, err := Fetch(context.Background(), FetchOptions{
				Source: source, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if seenTag != "light" {
				t.Fatalf("verifier tag = %q", seenTag)
			}
			if staged.Dir != filepath.Join(root, source.Name, "light") {
				t.Fatalf("Dir = %q, want %q", staged.Dir, filepath.Join(root, source.Name, "light"))
			}
			if !strings.HasPrefix(seenDir, staged.Dir) {
				t.Fatalf("verifier cache dir %q not under stage dir %q", seenDir, staged.Dir)
			}
			if staged.Commit != "0123456789abcdef0123456789abcdef01234567" {
				t.Fatalf("Commit = %q", staged.Commit)
			}
			if len(staged.Binaries) != len(source.Platforms) {
				t.Fatalf("staged %d binaries, want %d", len(staged.Binaries), len(source.Platforms))
			}
			for _, platform := range source.Platforms {
				path := staged.Binaries[platform]
				if path != filepath.Join(root, source.Name, "light", platform, source.Binary) {
					t.Fatalf("%s path = %q", platform, path)
				}
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				if string(content) != platform+"-binary" {
					t.Fatalf("%s content = %q, want %q", platform, content, platform+"-binary")
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("%s not executable: %v", platform, info.Mode())
				}
			}
			if entries, _ := filepath.Glob(filepath.Join(staged.Dir, "*", "*.partial")); len(entries) != 0 {
				t.Fatalf("partial files left behind: %v", entries)
			}
		})
	}
}

// TestFetchStagesOpnsensectlWithoutAStackBundle pins that a source without a
// stack bundle stages from its archives alone and reports no stack paths.
func TestFetchStagesOpnsensectlWithoutAStackBundle(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Opnsensectl)
	var seenTag, seenDir string
	verify := writingVerifier(t, Opnsensectl, completeRelease(t, Opnsensectl), &seenTag, &seenDir)
	root := t.TempDir()

	staged, err := Fetch(context.Background(), FetchOptions{
		Source: Opnsensectl, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if staged.StackDir != "" || staged.StackManifest != "" {
		t.Fatalf("StackDir = %q, StackManifest = %q, want both empty", staged.StackDir, staged.StackManifest)
	}
	if len(staged.Assets) != 0 {
		t.Fatalf("Assets = %v, want none without a manifest", staged.Assets)
	}
	if _, statErr := os.Stat(filepath.Join(staged.Dir, stackDirName)); statErr == nil {
		t.Fatal("a stack directory was created for a source without a bundle")
	}
}

func TestFetchDereferencesAnnotatedTag(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		t.Run(source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, source)
			var seenTag, seenDir string
			verify := writingVerifier(t, source, completeRelease(t, source), &seenTag, &seenDir)

			staged, err := Fetch(context.Background(), FetchOptions{
				Source: source, Tag: "annotated", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if staged.Commit != "fedcba9876543210fedcba9876543210fedcba98" {
				t.Fatalf("Commit = %q, want the dereferenced commit", staged.Commit)
			}
		})
	}
}

func TestFetchRejectsForeignArchiveMember(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		t.Run(source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, source)
			assets := completeRelease(t, source)
			assets[archiveName(source, source.Platforms[0])] = tarGz(t, map[string][]byte{
				source.Binary: []byte("x"), "../etc/passwd": []byte("root"),
			}, []string{source.Binary, "../etc/passwd"})
			var seenTag, seenDir string
			verify := writingVerifier(t, source, assets, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: source, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), `unexpected archive member "../etc/passwd"`) {
				t.Fatalf("Fetch error = %v, want the foreign member rejected", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "etc", "passwd")); statErr == nil {
				t.Fatal("foreign member was written")
			}
		})
	}
}

// TestFetchRejectsTheOtherSourcesBinary pins that the member allowlist follows
// the source: an archive carrying the other source's binary is refused, and
// nothing is placed where the binary would go.
func TestFetchRejectsTheOtherSourcesBinary(t *testing.T) {
	t.Parallel()
	for _, pair := range []struct{ source, other Source }{{Gateway, Opnsensectl}, {Opnsensectl, Gateway}} {
		t.Run(pair.source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, pair.source)
			platform := pair.source.Platforms[len(pair.source.Platforms)-1]
			assets := completeRelease(t, pair.source)
			assets[archiveName(pair.source, platform)] = binaryArchive(t, pair.other, "wrong binary")
			var seenTag, seenDir string
			verify := writingVerifier(t, pair.source, assets, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: pair.source, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			want := `unexpected archive member "` + pair.other.Binary + `"`
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), platform) {
				t.Fatalf("Fetch error = %v, want %s on %s", err, want, platform)
			}
			if _, statErr := os.Stat(filepath.Join(root, pair.source.Name, "light", platform, pair.source.Binary)); statErr == nil {
				t.Fatal("a binary was placed from the wrong archive")
			}
		})
	}
}

// TestFetchFailsWhenAnArchiveLacksTheBinary pins that an archive holding only
// a README names the binary it is missing.
func TestFetchFailsWhenAnArchiveLacksTheBinary(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		t.Run(source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, source)
			assets := completeRelease(t, source)
			assets[archiveName(source, source.Platforms[0])] = tarGz(t, map[string][]byte{"README.md": []byte("readme")}, []string{"README.md"})
			var seenTag, seenDir string
			verify := writingVerifier(t, source, assets, &seenTag, &seenDir)

			_, err := Fetch(context.Background(), FetchOptions{
				Source: source, Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), "archive has no "+source.Binary+" member") {
				t.Fatalf("Fetch error = %v, want the missing %s member named", err, source.Binary)
			}
		})
	}
}

// TestFetchFailsWhenAPlatformArchiveIsMissing pins that every platform the
// source names is required: dropping any one archive fails the stage and names
// that platform.
func TestFetchFailsWhenAPlatformArchiveIsMissing(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		for _, platform := range source.Platforms {
			t.Run(source.Name+"/"+platform, func(t *testing.T) {
				t.Parallel()
				server := tagAPI(t, source)
				assets := completeRelease(t, source)
				delete(assets, archiveName(source, platform))
				var seenTag, seenDir string
				verify := writingVerifier(t, source, assets, &seenTag, &seenDir)

				_, err := Fetch(context.Background(), FetchOptions{
					Source: source, Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
				})
				if err == nil || !strings.Contains(err.Error(), platform) {
					t.Fatalf("Fetch error = %v, want the missing %s archive named", err, platform)
				}
			})
		}
	}
}

func TestFetchRequiresTag(t *testing.T) {
	t.Parallel()
	_, err := Fetch(context.Background(), FetchOptions{Source: Gateway, CacheRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "tag is required") {
		t.Fatalf("Fetch error = %v", err)
	}
}

// TestFetchRefusesAnUnusableSource pins that a descriptor with no repository
// or platform, or with a segment that could escape the cache root, never
// reaches the verifier or creates anything under the cache root.
func TestFetchRefusesAnUnusableSource(t *testing.T) {
	t.Parallel()
	cases := map[string]Source{
		"zero":              {},
		"no repo":           {Name: "x", Binary: "x", Platforms: []string{"linux_amd64"}},
		"no platforms":      {Name: "x", Repo: "o/r", Binary: "x"},
		"escaping name":     {Name: "../x", Repo: "o/r", Binary: "x", Platforms: []string{"linux_amd64"}},
		"escaping binary":   {Name: "x", Repo: "o/r", Binary: "../x", Platforms: []string{"linux_amd64"}},
		"escaping platform": {Name: "x", Repo: "o/r", Binary: "x", Platforms: []string{"linux/amd64"}},
	}
	for label, source := range cases {
		root := t.TempDir()
		called := false
		_, err := Fetch(context.Background(), FetchOptions{
			Source: source, Tag: "light", CacheRoot: root,
			Verify: func(_ context.Context, _ selfupdate.Options, _ string) error { called = true; return nil },
		})
		if err == nil || !strings.Contains(err.Error(), "release: source") {
			t.Fatalf("%s: Fetch error = %v, want the source refused", label, err)
		}
		if called {
			t.Fatalf("%s: Fetch reached the verifier", label)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("%s: Fetch created %v under the cache root", label, entries)
		}
	}
}

// TestFetchStagesTheStackBundle pins the staging contract MWAN-433 adds: the
// bundle unpacks to <stage>/wanconfig-stack with the manifest at its root and
// each package under debs/, every sha256 matching the manifest.
func TestFetchStagesTheStackBundle(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Gateway)
	var seenTag, seenDir string
	verify := writingVerifier(t, Gateway, completeRelease(t, Gateway), &seenTag, &seenDir)

	staged, err := Fetch(context.Background(), FetchOptions{
		Source: Gateway, Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if staged.StackDir != filepath.Join(staged.Dir, "wanconfig-stack") {
		t.Fatalf("StackDir = %q", staged.StackDir)
	}
	if staged.StackManifest != filepath.Join(staged.StackDir, "manifest.txt") {
		t.Fatalf("StackManifest = %q", staged.StackManifest)
	}
	if len(staged.Assets) != 0 {
		t.Fatalf("Assets = %v, want none without a manifest", staged.Assets)
	}
	for name, want := range testStackDebs {
		content, err := os.ReadFile(filepath.Join(staged.StackDir, "debs", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !bytes.Equal(content, want) {
			t.Fatalf("%s content = %q, want %q", name, content, want)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(staged.StackDir, "*", "*.partial")); len(entries) != 0 {
		t.Fatalf("partial files left behind: %v", entries)
	}
}

// TestFetchFailsWhenTheStackBundleIsMissing pins that a tag from before the
// packaging build fails at stage time with a plain message, before any play.
func TestFetchFailsWhenTheStackBundleIsMissing(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Gateway)
	assets := completeRelease(t, Gateway)
	delete(assets, StackBundleAsset)
	var seenTag, seenDir string
	verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)

	_, err := Fetch(context.Background(), FetchOptions{
		Source: Gateway, Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err == nil || !strings.Contains(err.Error(), "ships no "+StackBundleAsset) {
		t.Fatalf("Fetch error = %v, want the missing bundle named", err)
	}
}

// TestFetchRejectsForeignStackBundleMember pins that a member outside the
// manifest-plus-debs contract stops the stage.
func TestFetchRejectsForeignStackBundleMember(t *testing.T) {
	t.Parallel()
	for _, member := range []string{"../../etc/passwd.deb", "debs/../escape.deb", "debs/sub/dir.deb", "notes.txt"} {
		t.Run(member, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			assets := completeRelease(t, Gateway)
			assets[StackBundleAsset] = tarGz(t, map[string][]byte{
				stackManifestName: []byte("# manifest\n"),
				member:            []byte("payload"),
			}, []string{stackManifestName, member})
			var seenTag, seenDir string
			verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), "unexpected stack bundle member") {
				t.Fatalf("Fetch error = %v, want the member rejected", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, "etc", "passwd.deb")); statErr == nil {
				t.Fatal("foreign member was written")
			}
		})
	}
}

// TestFetchFailsOnAStackChecksumMismatch pins that a package whose bytes do
// not match the manifest stops the stage.
func TestFetchFailsOnAStackChecksumMismatch(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Gateway)
	manifest := "libyang3 1.0 amd64 " + strings.Repeat("0", 64) + " debs/libyang3_1.0_amd64.deb\n"
	assets := completeRelease(t, Gateway)
	assets[StackBundleAsset] = tarGz(t, map[string][]byte{
		stackManifestName:             []byte(manifest),
		"debs/libyang3_1.0_amd64.deb": []byte("deb bytes"),
	}, []string{stackManifestName, "debs/libyang3_1.0_amd64.deb"})
	var seenTag, seenDir string
	verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)

	_, err := Fetch(context.Background(), FetchOptions{
		Source: Gateway, Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("Fetch error = %v, want the checksum mismatch named", err)
	}
}

// TestFetchRefusesTagThatCouldEscapeAPathOrURL pins that a tag never reaches
// the cache path or the API URL unless it is plain tag characters: nothing is
// created under the cache root and the verifier is never called.
func TestFetchRefusesTagThatCouldEscapeAPathOrURL(t *testing.T) {
	t.Parallel()
	for _, tag := range []string{"../../tmp/x", "a/b", "v1?x=1", "v1#frag", "..", "-lead", "v1..2", "with space"} {
		root := t.TempDir()
		called := false
		_, err := Fetch(context.Background(), FetchOptions{
			Source: Gateway, Tag: tag, CacheRoot: root,
			Verify: func(_ context.Context, _ selfupdate.Options, _ string) error { called = true; return nil },
		})
		if err == nil || !strings.Contains(err.Error(), "may only use") {
			t.Fatalf("Fetch(%q) error = %v, want the tag refused", tag, err)
		}
		if called {
			t.Fatalf("Fetch(%q) reached the verifier", tag)
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 0 {
			t.Fatalf("Fetch(%q) created %v under the cache root", tag, entries)
		}
	}
	for _, tag := range []string{"202608162055-5-8ce01a2", "v1.2.3", "v1.2.3-rc.1", "release_2026"} {
		if !tagPattern.MatchString(tag) {
			t.Fatalf("tagPattern rejects legitimate tag %q", tag)
		}
	}
}

// yangAsset is the schema asset a manifest-carrying gateway release publishes
// beside the stack bundle, with a nested member so the unpack layout is pinned.
const yangAsset = "mwan-yang_linux_amd64.tar.gz"

// testYangFiles are the members yangAsset carries, by member name.
var testYangFiles = map[string][]byte{
	"goodkind-mwan-steering@2026-09-01.yang": []byte("module steering"),
	"ietf/ietf-interfaces@2018-02-20.yang":   []byte("module interfaces"),
}

func testYangArchive(t *testing.T) []byte {
	t.Helper()
	return tarGz(t, testYangFiles, []string{"goodkind-mwan-steering@2026-09-01.yang", "ietf/ietf-interfaces@2018-02-20.yang"})
}

// manifestArchive builds the release manifest asset: one member,
// release-manifest.json, listing the given entries.
func manifestArchive(t *testing.T, entries []manifestEntry) []byte {
	t.Helper()
	content, err := json.Marshal(manifest{Assets: entries})
	if err != nil {
		t.Fatal(err)
	}
	return tarGz(t, map[string][]byte{manifestMemberName: content}, []string{manifestMemberName})
}

// testManifestEntries list the stack bundle under its existing deploy variable
// and the schema asset under a new one, the entries a gateway release carries.
var testManifestEntries = []manifestEntry{
	{Name: StackBundleAsset, Unpack: "wanconfig-stack", Var: "wanconfig_stack_dir"},
	{Name: yangAsset, Unpack: "yang", Var: "mwan_yang_dir"},
}

// manifestRelease is a release of source that carries a manifest listing the
// stack bundle and the schema asset, on top of its platform archives.
func manifestRelease(t *testing.T, source Source, entries []manifestEntry) map[string][]byte {
	t.Helper()
	assets := completeRelease(t, source)
	assets[StackBundleAsset] = testStackBundle(t)
	assets[yangAsset] = testYangArchive(t)
	assets[ManifestAsset] = manifestArchive(t, entries)
	return assets
}

// TestFetchStagesEveryManifestAsset pins the manifest staging contract for
// both sources: every listed archive unpacks to <stage>/<unpack> with its
// members intact, Assets maps each var to that directory, and the source's
// StackBundle flag is not consulted, so the stack paths stay empty even for
// the gateway.
func TestFetchStagesEveryManifestAsset(t *testing.T) {
	t.Parallel()
	for _, source := range fetchSources {
		t.Run(source.Name, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, source)
			var seenTag, seenDir string
			verify := writingVerifier(t, source, manifestRelease(t, source, testManifestEntries), &seenTag, &seenDir)
			root := t.TempDir()

			staged, err := Fetch(context.Background(), FetchOptions{
				Source: source, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			want := map[string]string{
				"wanconfig_stack_dir": filepath.Join(staged.Dir, "wanconfig-stack"),
				"mwan_yang_dir":     filepath.Join(staged.Dir, "yang"),
			}
			if len(staged.Assets) != len(want) {
				t.Fatalf("Assets = %v, want %v", staged.Assets, want)
			}
			for name, dir := range want {
				if staged.Assets[name] != dir {
					t.Fatalf("Assets[%s] = %q, want %q", name, staged.Assets[name], dir)
				}
			}
			if staged.StackDir != "" || staged.StackManifest != "" {
				t.Fatalf("StackDir = %q, StackManifest = %q, want both empty with a manifest", staged.StackDir, staged.StackManifest)
			}
			for name, content := range testStackDebs {
				got, err := os.ReadFile(filepath.Join(staged.Assets["wanconfig_stack_dir"], "debs", name))
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if !bytes.Equal(got, content) {
					t.Fatalf("%s content = %q, want %q", name, got, content)
				}
			}
			for name, content := range testYangFiles {
				got, err := os.ReadFile(filepath.Join(staged.Assets["mwan_yang_dir"], filepath.FromSlash(name)))
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if !bytes.Equal(got, content) {
					t.Fatalf("%s content = %q, want %q", name, got, content)
				}
			}
			if entries, _ := filepath.Glob(filepath.Join(staged.Dir, "*", "*", "*.partial")); len(entries) != 0 {
				t.Fatalf("partial files left behind: %v", entries)
			}
		})
	}
}

// TestFetchFailsWhenAManifestAssetIsMissing pins that a manifest naming an
// archive the release does not contain stops the stage and names the asset,
// before any listed asset is unpacked.
func TestFetchFailsWhenAManifestAssetIsMissing(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Gateway)
	assets := manifestRelease(t, Gateway, testManifestEntries)
	delete(assets, yangAsset)
	var seenTag, seenDir string
	verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)
	root := t.TempDir()

	_, err := Fetch(context.Background(), FetchOptions{
		Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err == nil || !strings.Contains(err.Error(), "does not contain "+yangAsset) {
		t.Fatalf("Fetch error = %v, want the missing asset named", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", "yang")); statErr == nil {
		t.Fatal("a directory was created for the missing asset")
	}
}

// TestFetchRefusesADuplicateManifestEntry pins that two entries sharing an
// unpack directory, a variable, or an asset name stop the stage before any
// asset is unpacked, and the message names the repeated value.
func TestFetchRefusesADuplicateManifestEntry(t *testing.T) {
	t.Parallel()
	cases := map[string]manifestEntry{
		"unpack": {Name: yangAsset, Unpack: "wanconfig-stack", Var: "mwan_yang_dir"},
		"var":    {Name: yangAsset, Unpack: "yang", Var: "wanconfig_stack_dir"},
		"name":   {Name: StackBundleAsset, Unpack: "yang", Var: "mwan_yang_dir"},
	}
	for field, second := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			entries := []manifestEntry{testManifestEntries[0], second}
			var seenTag, seenDir string
			verify := writingVerifier(t, Gateway, manifestRelease(t, Gateway, entries), &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), "release manifest") {
				t.Fatalf("Fetch error = %v, want the duplicate refused", err)
			}
			for _, dir := range []string{"wanconfig-stack", "yang"} {
				if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", dir)); statErr == nil {
					t.Fatalf("%s was unpacked from a refused manifest", dir)
				}
			}
		})
	}
}

// TestFetchRefusesAnUnusableManifestEntry pins that an entry the stage could
// not honor is refused: an asset the verifier never downloads, a name or
// directory the binaries already use, or a value that could escape the stage.
func TestFetchRefusesAnUnusableManifestEntry(t *testing.T) {
	t.Parallel()
	cases := map[string]manifestEntry{
		"not an archive":          {Name: "release-notes.json", Unpack: "notes", Var: "mwan_notes_dir"},
		"escaping name":           {Name: "../x.tar.gz", Unpack: "x", Var: "mwan_x_dir"},
		"the manifest itself":     {Name: ManifestAsset, Unpack: "x", Var: "mwan_x_dir"},
		"a platform archive":      {Name: archiveName(Gateway, "linux_amd64"), Unpack: "x", Var: "mwan_x_dir"},
		"unpack archives":         {Name: yangAsset, Unpack: "archives", Var: "mwan_yang_dir"},
		"unpack a platform":       {Name: yangAsset, Unpack: "linux_amd64", Var: "mwan_yang_dir"},
		"escaping unpack":         {Name: yangAsset, Unpack: "../yang", Var: "mwan_yang_dir"},
		"nested unpack":           {Name: yangAsset, Unpack: "a/b", Var: "mwan_yang_dir"},
		"empty unpack":            {Name: yangAsset, Unpack: "", Var: "mwan_yang_dir"},
		"var with a dash":         {Name: yangAsset, Unpack: "yang", Var: "mwan-models-dir"},
		"var starting with digit": {Name: yangAsset, Unpack: "yang", Var: "1models"},
		"empty var":               {Name: yangAsset, Unpack: "yang", Var: ""},
	}
	for label, entry := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			var seenTag, seenDir string
			verify := writingVerifier(t, Gateway, manifestRelease(t, Gateway, []manifestEntry{entry}), &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), "release manifest") {
				t.Fatalf("Fetch error = %v, want the entry refused", err)
			}
			entries, _ := os.ReadDir(filepath.Join(root, Gateway.Name, "light"))
			for _, dirEntry := range entries {
				if dirEntry.Name() != "archives" && dirEntry.Name() != "linux_amd64" {
					t.Fatalf("%s was created from a refused manifest", dirEntry.Name())
				}
			}
		})
	}
}

// TestFetchNeverParsesAnUnverifiedManifest pins that a manifest the verifier
// rejected is never read: the verifier writes the whole release, including a
// manifest whose content is not JSON, then reports a failed check, and the
// stage fails with that report rather than a parse error, unpacking nothing.
func TestFetchNeverParsesAnUnverifiedManifest(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"manifest attestation": "release attestation verification failed for " + ManifestAsset,
		"asset checksum":       "checksum mismatch for " + yangAsset,
	}
	for label, failure := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			assets := manifestRelease(t, Gateway, testManifestEntries)
			assets[ManifestAsset] = tarGz(t, map[string][]byte{manifestMemberName: []byte("not json")}, []string{manifestMemberName})
			var seenTag, seenDir string
			writing := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)
			verify := func(ctx context.Context, options selfupdate.Options, tag string) error {
				if err := writing(ctx, options, tag); err != nil {
					return err
				}
				return errors.New(failure)
			}
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("Fetch error = %v, want the verifier's failure", err)
			}
			if strings.Contains(err.Error(), manifestMemberName) {
				t.Fatalf("Fetch error = %v, the unverified manifest was parsed", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", "archives", ManifestAsset)); statErr != nil {
				t.Fatalf("the manifest archive was not written before the verifier failed: %v", statErr)
			}
			for _, dir := range []string{"wanconfig-stack", "yang", "linux_amd64"} {
				if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", dir)); statErr == nil {
					t.Fatalf("%s was unpacked after the verifier failed", dir)
				}
			}
		})
	}
}

// TestFetchDiscardsAManifestTheVerifierDidNotWrite pins that an archive left
// under the stage by an earlier run is not read as verified: a stale manifest
// sits in the archive directory, the verifier writes a release without one,
// and the stage proceeds as a release without a manifest.
func TestFetchDiscardsAManifestTheVerifierDidNotWrite(t *testing.T) {
	t.Parallel()
	server := tagAPI(t, Gateway)
	root := t.TempDir()
	archiveDir := filepath.Join(root, Gateway.Name, "light", "archives")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := manifestArchive(t, testManifestEntries)
	if err := os.WriteFile(filepath.Join(archiveDir, ManifestAsset), stale, 0o644); err != nil {
		t.Fatal(err)
	}
	var seenTag, seenDir string
	verify := writingVerifier(t, Gateway, completeRelease(t, Gateway), &seenTag, &seenDir)

	staged, err := Fetch(context.Background(), FetchOptions{
		Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(staged.Assets) != 0 {
		t.Fatalf("Assets = %v from a manifest the verifier did not write", staged.Assets)
	}
	if staged.StackDir != filepath.Join(staged.Dir, "wanconfig-stack") {
		t.Fatalf("StackDir = %q, want the bundle staged as a release without a manifest", staged.StackDir)
	}
	if _, statErr := os.Stat(filepath.Join(archiveDir, ManifestAsset)); statErr == nil {
		t.Fatal("the stale manifest archive survived the stage")
	}
}

// TestFetchRefusesAManifestArchiveWithTheWrongMember pins that the manifest
// archive must carry exactly release-manifest.json and nothing else.
func TestFetchRefusesAManifestArchiveWithTheWrongMember(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(manifest{Assets: testManifestEntries})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"other member": tarGz(t, map[string][]byte{"notes.txt": valid}, []string{"notes.txt"}),
		"two members":  tarGz(t, map[string][]byte{manifestMemberName: valid, "extra.json": valid}, []string{manifestMemberName, "extra.json"}),
		"no members":   tarGz(t, map[string][]byte{}, nil),
		"not json":     tarGz(t, map[string][]byte{manifestMemberName: []byte("{")}, []string{manifestMemberName}),
	}
	for label, archive := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			assets := manifestRelease(t, Gateway, testManifestEntries)
			assets[ManifestAsset] = archive
			var seenTag, seenDir string
			verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), ManifestAsset) && !strings.Contains(err.Error(), manifestMemberName) {
				t.Fatalf("Fetch error = %v, want the manifest archive refused", err)
			}
			for _, dir := range []string{"wanconfig-stack", "yang"} {
				if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", dir)); statErr == nil {
					t.Fatalf("%s was unpacked from a refused manifest", dir)
				}
			}
		})
	}
}

// TestFetchRejectsForeignManifestAssetMember pins that a listed asset's
// member can never land outside the asset's directory: an escaping path, an
// absolute path, and a symbolic link each stop the stage.
func TestFetchRejectsForeignManifestAssetMember(t *testing.T) {
	t.Parallel()
	symlink := func(t *testing.T) []byte {
		t.Helper()
		var buffer bytes.Buffer
		gzipWriter := gzip.NewWriter(&buffer)
		tarWriter := tar.NewWriter(gzipWriter)
		header := &tar.Header{Name: "link.yang", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if err := tarWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	cases := map[string][]byte{
		"dot-dot":  tarGz(t, map[string][]byte{"../escape.yang": []byte("x")}, []string{"../escape.yang"}),
		"absolute": tarGz(t, map[string][]byte{"/etc/escape.yang": []byte("x")}, []string{"/etc/escape.yang"}),
		"symlink":  symlink(t),
		"empty":    tarGz(t, map[string][]byte{}, nil),
	}
	for label, archive := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			server := tagAPI(t, Gateway)
			assets := manifestRelease(t, Gateway, testManifestEntries)
			assets[yangAsset] = archive
			var seenTag, seenDir string
			verify := writingVerifier(t, Gateway, assets, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Source: Gateway, Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
			})
			if err == nil || !strings.Contains(err.Error(), yangAsset) {
				t.Fatalf("Fetch error = %v, want the asset member rejected", err)
			}
			if _, statErr := os.Stat(filepath.Join(root, Gateway.Name, "light", "escape.yang")); statErr == nil {
				t.Fatal("foreign member was written")
			}
			if _, statErr := os.Stat(filepath.Join(root, "etc", "escape.yang")); statErr == nil {
				t.Fatal("foreign member was written")
			}
		})
	}
}
