package main_test

import (
	"bytes"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// bucket serves encrypted state objects and paginated S3 workspace listings.
type bucket struct {
	mutex      sync.Mutex
	objects    map[string][]byte
	token      string
	deniedPage int
	requested  map[string]int
}

func (b *bucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if r.Header.Get("X-Amz-Security-Token") != b.token ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		b.deny(w)

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		b.requested["mutation"]++
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	if r.URL.Query().Get("list-type") == "2" && strings.TrimRight(r.URL.Path, "/") == "/fixture" {
		b.list(w, r)

		return
	}

	key, ok := strings.CutPrefix(r.URL.Path, "/fixture/")
	data, exists := b.objects[key]
	if !ok || !exists {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	b.requested[key]++
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"12345678901234567890123456789012"`)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (b *bucket) deny(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusForbidden)
	_ = xml.NewEncoder(w).Encode(
		struct {
			XMLName xml.Name `xml:"Error"`
			Code    string   `xml:"Code"`
		}{
			Code: "AccessDenied",
		},
	)
}

func (b *bucket) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var keys []string

	for key := range b.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	slices.Sort(keys)
	index := 0
	token := r.URL.Query().Get("continuation-token")
	if token != "" {
		var err error
		index, err = strconv.Atoi(token)
		if err != nil || index < 0 || index >= len(keys) {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
	}

	page := index + 1
	b.requested[fmt.Sprintf("page-%d", page)]++
	if page == b.deniedPage {
		b.deny(w)

		return
	}

	result := struct {
		XMLName   xml.Name `xml:"ListBucketResult"`
		Truncated bool     `xml:"IsTruncated"`
		Next      string   `xml:"NextContinuationToken,omitempty"`
		Keys      []string `xml:"Contents>Key"`
	}{}
	if index < len(keys) {
		result.Keys = []string{keys[index]}
		result.Truncated = index+1 < len(keys)
	}

	if result.Truncated {
		result.Next = strconv.Itoa(page)
	}

	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(result)
}

func TestBucketWorkspaceAccess(t *testing.T) {
	repo := t.TempDir()
	root := filepath.Join(repo, "opentofu")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("TF_VAR_vault_tofu_state_passphrase", rand.Text())
	t.Setenv("TF_VAR_fixture_value", "fixture")
	t.Setenv("AWS_ACCESS_KEY_ID", rand.Text())
	t.Setenv("AWS_SECRET_ACCESS_KEY", rand.Text())
	session := rand.Text()
	t.Setenv("AWS_SESSION_TOKEN", session)
	retirementRun(t, repo, "git", "init", "-q")
	retirementSave(t, filepath.Join(repo, ".gitignore"), []byte(".terraform/\n*.tfstate*\n"))
	retirementSave(
		t,
		filepath.Join(repo, "configsctl.yml"),
		[]byte("tofu:\n  workspaces_dir: opentofu\n"),
	)
	retirementBackend(t, root)
	retirementInstall(t, root, "records.tf", "records.tf")
	retirementRun(t, root, "tofu", "init", "-input=false")
	retirementRun(t, root, "tofu", "apply", "-input=false", "-auto-approve")
	retirementRun(t, root, "tofu", "workspace", "new", "review")
	retirementRun(t, root, "tofu", "apply", "-input=false", "-auto-approve")
	const defaultKey = "state/root.tfstate"
	const namedKey = "workspaces/review/" + defaultKey
	store := &bucket{objects: map[string][]byte{}, requested: map[string]int{}, token: session}

	for key, path := range map[string]string{
		defaultKey: "runtime.tfstate",
		namedKey:   "terraform.tfstate.d/review/terraform.tfstate",
	} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || !bytes.Contains(data, []byte("encrypted_data")) {
			t.Fatal("fixture must contain readable encrypted state")
		}

		store.objects[key] = data
	}

	store.objects["workspaces/other/unrelated.tfstate"] = store.objects[defaultKey]
	server := httptest.NewServer(store)
	defer server.Close()
	installS3Backend(t, root, server.URL)
	retirementInstall(t, root, "removed.tf", "moved.tf")
	base := retirementCommit(t, repo)
	retirementDrop(t, filepath.Join(root, "removed.tf"))
	head := retirementCommit(t, repo)

	for _, denied := range []int{1, 2, 0} {
		store.mutex.Lock()
		store.deniedPage = denied
		store.requested = map[string]int{}
		store.mutex.Unlock()
		err := verifyRetirement(t, repo, base, head)
		want := "cannot list every S3 workspace state object"
		if denied == 0 {
			want = `workspace \"review\" still contains a retired address`
		}

		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("denied page %d: wanted %q, got %v", denied, want, err)
		}

		store.mutex.Lock()
		reads := store.requested[defaultKey]
		writes := store.requested["mutation"]
		store.mutex.Unlock()
		if reads == 0 || writes != 0 {
			t.Fatalf("default reads %d, mutations %d", reads, writes)
		}
	}
}

func installS3Backend(t *testing.T, root, endpoint string) {
	t.Helper()
	filename := filepath.Join(root, "backend.tf")
	source, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	config, diag := hclwrite.ParseConfig(source, filename, hcl.InitialPos)
	if diag.HasErrors() {
		t.Fatal("invalid local backend fixture")
	}

	terraform := config.Body().FirstMatchingBlock("terraform", nil).Body()
	terraform.RemoveBlock(terraform.FirstMatchingBlock("backend", []string{"local"}))
	retirementSave(t, filename, config.Bytes())
	data, err := os.ReadFile(filepath.Join("testdata", "retirement", "s3.tf.tmpl"))
	if err != nil {
		t.Fatal(err)
	}

	retirementSave(
		t,
		filepath.Join(root, "remote.tf"),
		bytes.ReplaceAll(data, []byte("ENDPOINT"), []byte(endpoint)),
	)
}
