//go:build unix

package installedsource_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/configsctl/internal/installedsource"
	"goodkind.io/configsctl/internal/tofuplansource"
)

const (
	fixtureRevision = "0123456789012345678901234567890123456789"
	fixtureManifest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type fixture struct {
	settings installedsource.Settings
	document map[string]any
	files    []map[string]any
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	settings := installedsource.Settings{Root: root, Workspace: "poweredge/idrac-certificate", Node: "poweredge", Environment: "testbed"}
	inputs := map[string]string{
		"configsctl.yml":                "tofu:\n  workspaces_dir: workspace\n",
		"workspace/main.tf":             "output \"source\" { value = \"installed\" }\n",
		"workspace/.terraform.lock.hcl": "# A nonsecret provider lock.\n",
		"workspace/item/main.tf":        "variable \"value\" { type = string }\n",
		"native/main.tf":                "output \"lock_targets\" { value = {} }\n",
		"operations/backend.hcl":        "address = \"https://localhost/state\"\n",
	}
	files := make([]map[string]any, 0, len(inputs))
	for relative, content := range inputs {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(content))
		files = append(files, map[string]any{
			"path": path, "present": 1, "matches": true, "size": len(content),
			"sha256": hex.EncodeToString(digest[:]), "mode": "0600", "uid": os.Geteuid(), "gid": os.Getegid(),
		})
	}
	installed := map[string]any{
		"workspace": settings.Workspace, "node": settings.Node, "environment": settings.Environment,
		"source_revision": fixtureRevision, "manifest_sha256": fixtureManifest,
	}
	directory := map[string]any{
		"path": filepath.Join(root, "workspace"), "present": true, "matches": 1,
		"mode": "0700", "uid": os.Geteuid(), "gid": os.Getegid(),
	}
	return fixture{settings: settings, files: files, document: map[string]any{
		"workspace": settings.Workspace, "node": settings.Node, "environment": settings.Environment,
		"state": "committed", "installed": installed, "files": files, "directories": []map[string]any{directory},
	}}
}

func (f fixture) verify(t *testing.T) (*installedsource.Source, error) {
	t.Helper()
	data, err := json.Marshal(f.document)
	if err != nil {
		t.Fatal(err)
	}
	return installedsource.Verify(f.settings, f.settings.Root, data, []string{"workspace", "native"}, []string{"configsctl.yml"})
}

func TestVerifyAndCopyInstalledInputs(t *testing.T) {
	f := newFixture(t)
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}
	if source.Identity.Workspace != f.settings.Workspace || source.Identity.SourceRevision != fixtureRevision {
		t.Fatalf("the verified source returned the wrong installation identity: %+v", source.Identity)
	}
	destination := t.TempDir()
	if err := source.Copy(destination, []string{"configsctl.yml", "operations/backend.hcl"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"configsctl.yml", "workspace/main.tf", "workspace/.terraform.lock.hcl", "workspace/item/main.tf", "native/main.tf", "operations/backend.hcl"} {
		original, err := os.ReadFile(filepath.Join(f.settings.Root, path))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(filepath.Join(destination, path))
		if err != nil || string(copied) != string(original) {
			t.Fatalf("copied input %s does not match the installed bytes: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "operations", "unrelated")); !os.IsNotExist(err) {
		t.Fatalf("the copy unexpectedly created an unrelated operational file: %v", err)
	}
}

func TestVerifyRefusesChangedInstalledInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, fixture)
		want   string
	}{
		{name: "bytes", change: func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.settings.Root, "workspace/main.tf"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "changed after the API observation"},
		{name: "mode", change: func(t *testing.T, f fixture) {
			if err := os.Chmod(filepath.Join(f.settings.Root, "workspace/main.tf"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "observed mode"},
		{name: "owner", change: func(_ *testing.T, f fixture) {
			for _, file := range f.files {
				file["uid"] = os.Geteuid() + 1
			}
		}, want: "observed owner and group"},
		{name: "unlisted override", change: func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.settings.Root, "workspace/override.tf"), []byte("output \"injected\" { value = true }"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "not in the installed file set"},
		{name: "unlisted variable file", change: func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.settings.Root, "workspace/override.auto.tfvars.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "not in the installed file set"},
		{name: "symlink", change: func(t *testing.T, f fixture) {
			if err := os.Symlink("main.tf", filepath.Join(f.settings.Root, "workspace/override.tf")); err != nil {
				t.Fatal(err)
			}
		}, want: "symbolic links"},
		{name: "directory metadata", change: func(t *testing.T, f fixture) {
			if err := os.Chmod(filepath.Join(f.settings.Root, "workspace"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, want: "observed mode"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			test.change(t, f)
			_, err := f.verify(t)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("verification error = %v; want %q", err, test.want)
			}
		})
	}
}

func TestVerifyRefusesInvalidInstallationIdentity(t *testing.T) {
	for _, key := range []string{"workspace", "node", "environment"} {
		for _, saved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s saved=%t", key, saved), func(t *testing.T) {
				f := newFixture(t)
				if saved {
					installed, valid := f.document["installed"].(map[string]any)
					if !valid {
						t.Fatal("the fixture installation record is invalid")
					}
					delete(installed, key)
				} else {
					f.document[key] = "different"
				}
				if _, err := f.verify(t); err == nil {
					t.Fatal("verification accepted a mismatched or absent installation identity")
				}
			})
		}
	}
	for _, state := range []string{"interrupted", "none"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			f.document["state"] = state
			if _, err := f.verify(t); err == nil {
				t.Fatalf("verification accepted installation state %q", state)
			}
		})
	}
	t.Run("root", func(t *testing.T) {
		f := newFixture(t)
		f.settings.Root = filepath.Dir(f.settings.Root)
		data, err := json.Marshal(f.document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := installedsource.Verify(f.settings, t.TempDir(), data, nil, nil); err == nil {
			t.Fatal("verification accepted a different working directory")
		}
	})
}

func TestCopyRechecksInstalledBytes(t *testing.T) {
	f := newFixture(t)
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.settings.Root, "native/main.tf"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := source.Copy(t.TempDir(), []string{"configsctl.yml"}); err == nil {
		t.Fatal("the source copy accepted changed installed bytes")
	}
}

func (f fixture) replaceFile(t *testing.T, relative, content string) {
	t.Helper()
	path := filepath.Join(f.settings.Root, relative)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	for _, file := range f.files {
		if file["path"] == path {
			file["size"] = len(content)
			file["sha256"] = hex.EncodeToString(digest[:])
			return
		}
	}
	t.Fatalf("the fixture does not contain %s", relative)
}

func runTofu(t *testing.T, root, data string, args ...string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "tofu", args...)
	command.Dir = filepath.Join(root, "workspace")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"TF_DATA_DIR=" + data, "TF_IN_AUTOMATION=1",
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("OpenTofu %s failed against verified installed inputs: %v\n%s", args[0], err, output)
	}
}

func TestVerifiedCopyPlansWithRealOpenTofu(t *testing.T) {
	if _, err := exec.LookPath("tofu"); err != nil {
		t.Skip("OpenTofu is not installed")
	}
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	f := newFixture(t)
	state := filepath.Join(t.TempDir(), "state.tfstate")
	configuration := fmt.Sprintf("terraform {\n  backend \"local\" { path = %q }\n}\nmodule \"item\" {\n  source = \"./item\"\n  value = \"installed\"\n}\n", state)
	f.replaceFile(t, "workspace/main.tf", configuration)
	f.replaceFile(t, "workspace/item/main.tf", "variable \"value\" { type = string }\nresource \"terraform_data\" \"item\" { input = var.value }\n")
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}
	firstCopy := t.TempDir()
	if err := source.RequirePersistentBackend("workspace", "default", firstCopy); err != nil {
		t.Fatal(err)
	}
	if err := source.Copy(firstCopy, []string{"configsctl.yml"}); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	runTofu(t, firstCopy, data, "init", "-input=false", "-lockfile=readonly")
	secondCopy := t.TempDir()
	if err := source.Copy(secondCopy, []string{"configsctl.yml"}); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(t.TempDir(), "installed.tfplan")
	runTofu(t, secondCopy, data, "plan", "-input=false", "-out="+plan)
	identity := source.Identity
	recorded := tofuplansource.Source{
		Status: "committed", Commit: identity.SourceRevision, Workspace: "workspace", Kind: "installed",
		Root: identity.Root, Artifact: identity.Workspace, Node: identity.Node,
		Environment: identity.Environment, ManifestSHA256: identity.ManifestSHA256,
	}
	if err := tofuplansource.Record(plan, recorded); err != nil {
		t.Fatal(err)
	}
	if err := tofuplansource.Require(plan, recorded); err != nil {
		t.Fatal(err)
	}
	installed, valid := f.document["installed"].(map[string]any)
	if !valid {
		t.Fatal("the fixture installation record is invalid")
	}
	installed["manifest_sha256"] = strings.Repeat("b", 64)
	replacement, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}
	current := recorded
	current.ManifestSHA256 = replacement.Identity.ManifestSHA256
	if err := tofuplansource.Require(plan, current); err == nil {
		t.Fatal("the source cache accepted a plan from a replaced installed artifact")
	}
}

func TestInstalledBackendRequiresPersistentState(t *testing.T) {
	cases := []struct {
		name          string
		configuration string
		selected      string
		refused       bool
	}{
		{name: "relative local state", configuration: "terraform {\n backend \"local\" { path = \"state.tfstate\" }\n}\n", selected: "default", refused: true},
		{name: "default local state", configuration: "terraform {\n backend \"local\" {}\n}\n", selected: "default", refused: true},
		{name: "absolute local state", configuration: "terraform {\n backend \"local\" { path = \"/var/lib/configs/state.tfstate\" }\n}\n", selected: "default", refused: false},
		{name: "relative named workspace", configuration: "terraform {\n backend \"local\" { path = \"/var/lib/configs/state.tfstate\" }\n}\n", selected: "production", refused: true},
		{name: "remote state", configuration: "terraform {\n backend \"http\" {}\n}\n", selected: "default", refused: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.replaceFile(t, "workspace/main.tf", test.configuration)
			source, err := f.verify(t)
			if err != nil {
				t.Fatal(err)
			}
			err = source.RequirePersistentBackend("workspace", test.selected, t.TempDir())
			if test.refused {
				if err == nil || !strings.Contains(err.Error(), "set the local backend") {
					t.Fatalf("the installed backend did not require an absolute persistent path: %v", err)
				}
			} else if err != nil {
				t.Fatalf("the installed backend refused persistent state storage: %v", err)
			}
		})
	}
}
