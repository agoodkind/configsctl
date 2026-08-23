package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// writingVerifier stands in for the network verifier: it writes the given
// archives into the cache directory the way selfupdate does after verifying,
// and records the tag and directory it was asked about.
func writingVerifier(t *testing.T, archives map[string][]byte, seenTag *string, seenDir *string) Verifier {
	t.Helper()
	return func(_ context.Context, options selfupdate.Options, tag string) error {
		*seenTag = tag
		*seenDir = options.CacheDir
		if options.Config.Repo != Repo || options.Config.Binary != Binary {
			t.Fatalf("verifier got repo=%q binary=%q", options.Config.Repo, options.Config.Binary)
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
// annotated tag.
func tagAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/git/ref/tags/light", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "0123456789abcdef0123456789abcdef01234567", Type: gitObjectType("commit")}})
	})
	mux.HandleFunc("/repos/"+Repo+"/git/ref/tags/annotated", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "tagobject", Type: gitObjectTypeTag}})
	})
	mux.HandleFunc("/repos/"+Repo+"/git/tags/tagobject", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(gitRefResponse{Object: gitObject{SHA: "fedcba9876543210fedcba9876543210fedcba98", Type: gitObjectType("commit")}})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestFetchStagesEveryPlatformBinary(t *testing.T) {
	t.Parallel()
	server := tagAPI(t)
	linux := tarGz(t, map[string][]byte{Binary: []byte("linux-elf"), "README.md": []byte("readme")}, []string{"README.md", Binary})
	freebsd := tarGz(t, map[string][]byte{Binary: []byte("freebsd-elf")}, []string{Binary})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   linux,
		"mwan_freebsd_amd64.tar.gz": freebsd,
		StackBundleAsset:            testStackBundle(t),
	}, &seenTag, &seenDir)
	root := t.TempDir()

	staged, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if seenTag != "light" {
		t.Fatalf("verifier tag = %q", seenTag)
	}
	if !strings.HasPrefix(seenDir, staged.Dir) {
		t.Fatalf("verifier cache dir %q not under stage dir %q", seenDir, staged.Dir)
	}
	if staged.Commit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("Commit = %q", staged.Commit)
	}
	for platform, want := range map[string]string{"linux_amd64": "linux-elf", "freebsd_amd64": "freebsd-elf"} {
		path := staged.Binaries[platform]
		if path != filepath.Join(staged.Dir, platform, Binary) {
			t.Fatalf("%s path = %q", platform, path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(content) != want {
			t.Fatalf("%s content = %q, want %q", platform, content, want)
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
}

func TestFetchDereferencesAnnotatedTag(t *testing.T) {
	t.Parallel()
	server := tagAPI(t)
	archive := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   archive,
		"mwan_freebsd_amd64.tar.gz": archive,
		StackBundleAsset:            testStackBundle(t),
	}, &seenTag, &seenDir)

	staged, err := Fetch(context.Background(), FetchOptions{
		Tag: "annotated", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if staged.Commit != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Fatalf("Commit = %q, want the dereferenced commit", staged.Commit)
	}
}

func TestFetchRejectsForeignArchiveMember(t *testing.T) {
	t.Parallel()
	server := tagAPI(t)
	good := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	bad := tarGz(t, map[string][]byte{Binary: []byte("x"), "../etc/passwd": []byte("root")}, []string{Binary, "../etc/passwd"})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   good,
		"mwan_freebsd_amd64.tar.gz": bad,
	}, &seenTag, &seenDir)
	root := t.TempDir()

	_, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err == nil || !strings.Contains(err.Error(), `unexpected archive member "../etc/passwd"`) {
		t.Fatalf("Fetch error = %v, want the foreign member rejected", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "etc", "passwd")); statErr == nil {
		t.Fatal("foreign member was written")
	}
}

func TestFetchFailsWhenAPlatformArchiveIsMissing(t *testing.T) {
	t.Parallel()
	server := tagAPI(t)
	archive := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{"mwan_linux_amd64.tar.gz": archive}, &seenTag, &seenDir)

	_, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
	})
	if err == nil || !strings.Contains(err.Error(), "freebsd_amd64") {
		t.Fatalf("Fetch error = %v, want the missing freebsd archive named", err)
	}
}

func TestFetchRequiresTag(t *testing.T) {
	t.Parallel()
	_, err := Fetch(context.Background(), FetchOptions{CacheRoot: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "tag is required") {
		t.Fatalf("Fetch error = %v", err)
	}
}

// TestFetchStagesTheStackBundle pins the staging contract MWAN-433 adds: the
// bundle unpacks to <stage>/wanconfig-stack with the manifest at its root and
// each package under debs/, every sha256 matching the manifest.
func TestFetchStagesTheStackBundle(t *testing.T) {
	t.Parallel()
	server := tagAPI(t)
	binary := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   binary,
		"mwan_freebsd_amd64.tar.gz": binary,
		StackBundleAsset:            testStackBundle(t),
	}, &seenTag, &seenDir)

	staged, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
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
	server := tagAPI(t)
	binary := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   binary,
		"mwan_freebsd_amd64.tar.gz": binary,
	}, &seenTag, &seenDir)

	_, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
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
			server := tagAPI(t)
			binary := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
			bundle := tarGz(t, map[string][]byte{
				stackManifestName: []byte("# manifest\n"),
				member:            []byte("payload"),
			}, []string{stackManifestName, member})
			var seenTag, seenDir string
			verify := writingVerifier(t, map[string][]byte{
				"mwan_linux_amd64.tar.gz":   binary,
				"mwan_freebsd_amd64.tar.gz": binary,
				StackBundleAsset:            bundle,
			}, &seenTag, &seenDir)
			root := t.TempDir()

			_, err := Fetch(context.Background(), FetchOptions{
				Tag: "light", CacheRoot: root, APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
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
	server := tagAPI(t)
	binary := tarGz(t, map[string][]byte{Binary: []byte("x")}, []string{Binary})
	manifest := "libyang3 1.0 amd64 " + strings.Repeat("0", 64) + " debs/libyang3_1.0_amd64.deb\n"
	bundle := tarGz(t, map[string][]byte{
		stackManifestName:             []byte(manifest),
		"debs/libyang3_1.0_amd64.deb": []byte("deb bytes"),
	}, []string{stackManifestName, "debs/libyang3_1.0_amd64.deb"})
	var seenTag, seenDir string
	verify := writingVerifier(t, map[string][]byte{
		"mwan_linux_amd64.tar.gz":   binary,
		"mwan_freebsd_amd64.tar.gz": binary,
		StackBundleAsset:            bundle,
	}, &seenTag, &seenDir)

	_, err := Fetch(context.Background(), FetchOptions{
		Tag: "light", CacheRoot: t.TempDir(), APIBaseURL: server.URL, Client: server.Client(), Verify: verify,
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
			Tag: tag, CacheRoot: root,
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
