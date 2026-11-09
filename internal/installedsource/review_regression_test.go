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
)

func (f *fixture) addFile(t *testing.T, relative, content string) {
	t.Helper()
	path := filepath.Join(f.settings.Root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte(content))
	f.files = append(
		f.files,
		map[string]any{
			"path":    path,
			"present": true,
			"matches": true,
			"size":    len(content),
			"sha256":  hex.EncodeToString(digest[:]),
			"mode":    "0600",
			"uid":     os.Geteuid(),
			"gid":     os.Getegid(),
		},
	)
	f.document["files"] = f.files
}

func tofuExecutable(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("tofu")
	if err != nil {
		t.Skip("OpenTofu is not installed")
	}

	return binary
}

func reviewSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func tofuAt(t *testing.T, root, data string, environment []string, args ...string) []byte {
	t.Helper()
	command := exec.CommandContext(t.Context(), tofuExecutable(t), args...)
	command.Dir = root
	command.Env = append(
		[]string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"TF_DATA_DIR=" + data,
			"TF_IN_AUTOMATION=1",
		},
		environment...,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real OpenTofu %s failed: %v\n%s", args[0], err, output)
	}

	return output
}

type reviewedPlan struct {
	PlannedValues struct {
		Outputs map[string]struct {
			Value json.RawMessage `json:"value"`
		} `json:"outputs"`
	} `json:"planned_values"`
	ResourceChanges []struct {
		Address string `json:"address"`
	} `json:"resource_changes"`
}

func readReviewedPlan(t *testing.T, root, data, path string) reviewedPlan {
	t.Helper()
	output := tofuAt(t, root, data, nil, "show", "-json", path)
	var document reviewedPlan
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatal(err)
	}

	return document
}

func TestTargetAndExcludeFilesUseVerifiedCopies(t *testing.T) {
	for _, option := range []string{"target-file", "exclude-file"} {
		t.Run(
			option,
			func(t *testing.T) {
				f := newFixture(t)
				state := filepath.Join(t.TempDir(), "state.tfstate")
				f.replaceFile(
					t,
					"workspace/main.tf",
					fmt.Sprintf(reviewSource(t, "targeted.tf"), state),
				)
				f.addFile(t, "operations/addresses.txt", "terraform_data.item\n")
				source, err := f.verify(t)
				if err != nil {
					t.Fatal(err)
				}

				copyRoot := t.TempDir()
				copied, err := source.CopyFileArgument(
					copyRoot,
					"workspace",
					option,
					"../operations/addresses.txt",
					nil,
				)
				if err != nil {
					t.Fatal(err)
				}

				root := filepath.Join(copyRoot, "workspace")
				data := t.TempDir()
				tofuAt(t, root, data, nil, "init", "-input=false", "-lockfile=readonly")
				plan := filepath.Join(t.TempDir(), "targeted.tfplan")
				tofuAt(
					t,
					root,
					data,
					nil,
					"plan",
					"-input=false",
					"-"+option+"="+copied.Path,
					"-out="+plan,
				)
				result := readReviewedPlan(t, root, data, plan)
				wanted := "terraform_data.item"
				if option == "exclude-file" {
					wanted = "terraform_data.other"
				}

				if len(result.ResourceChanges) != 1 || result.ResourceChanges[0].Address != wanted {
					t.Fatalf("the verified %s did not select %s", option, wanted)
				}

				outside := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(
					outside,
					[]byte("terraform_data.other\n"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}

				if _, err := source.CopyFileArgument(
					copyRoot,
					"workspace",
					option,
					outside,
					nil,
				); err == nil {
					t.Fatalf("the %s accepted an outside file", option)
				}

				if err := os.WriteFile(
					copied.Original,
					[]byte("terraform_data.other\n"),
					0o600,
				); err != nil {
					t.Fatal(err)
				}

				if _, err := source.CopyFileArgument(
					copyRoot,
					"workspace",
					option,
					"../operations/addresses.txt",
					nil,
				); err == nil {
					t.Fatalf("the %s accepted bytes that changed after the API observation", option)
				}
			},
		)
	}
}

func TestInstalledSourceUsesOpenTofuFilePrecedence(t *testing.T) {
	for _, format := range []string{"native", "json"} {
		t.Run(
			format,
			func(t *testing.T) {
				f := newFixture(t)
				state := filepath.Join(t.TempDir(), "state.tfstate")
				name := "workspace/main.tofu"
				content := fmt.Sprintf(reviewSource(t, "preferred.tofu"), state)
				if format == "json" {
					name = "workspace/main.tofu.json"
					content = fmt.Sprintf(reviewSource(t, "preferred.tofu.json"), state)
					f.addFile(t, "workspace/main.tf.json", "invalid ignored JSON")
					f.replaceFile(t, "workspace/main.tf", "")
				} else {
					f.replaceFile(t, "workspace/main.tf", "invalid ignored HCL")
				}

				f.addFile(t, name, content)
				source, err := f.verify(t)
				if err != nil {
					t.Fatal(err)
				}

				if err := source.CheckModuleSources("workspace", "", ""); err != nil {
					t.Fatal(err)
				}

				copyRoot := t.TempDir()
				if err := source.RequirePersistentBackend(
					"workspace",
					"default",
					copyRoot,
				); err != nil {
					t.Fatal(err)
				}

				if err := source.Copy(copyRoot, nil); err != nil {
					t.Fatal(err)
				}

				root := filepath.Join(copyRoot, "workspace")
				data := t.TempDir()
				tofuAt(t, root, data, nil, "init", "-input=false", "-lockfile=readonly")
				tofuAt(t, root, data, nil, "plan", "-input=false")
				f.replaceFile(t, name, strings.Replace(content, state, "relative.tfstate", 1))
				changed, err := f.verify(t)
				if err != nil {
					t.Fatal(err)
				}

				if err := changed.RequirePersistentBackend(
					"workspace",
					"default",
					copyRoot,
				); err == nil {
					t.Fatalf("the %s backend bypassed persistent-state validation", name)
				}

				outside := t.TempDir()
				unsafe := fmt.Sprintf(reviewSource(t, "source-escape.tofu"), outside)
				if format == "json" {
					unsafe = fmt.Sprintf(reviewSource(t, "source-escape.tofu.json"), outside)
				}

				f.replaceFile(t, name, unsafe)
				unsafeSource, err := f.verify(t)
				if err != nil {
					t.Fatal(err)
				}

				if err := unsafeSource.CheckModuleSources("workspace", "", ""); err == nil {
					t.Fatalf("the %s module source bypassed verified-tree checks", name)
				}
			},
		)
	}
}

func TestWorkspaceTransitionChecksDestinationBackend(t *testing.T) {
	f := newFixture(t)
	state := filepath.Join(t.TempDir(), "state.tfstate")
	f.replaceFile(t, "workspace/main.tf", fmt.Sprintf(reviewSource(t, "local-backend.tf"), state))
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}

	copyRoot := t.TempDir()
	if err := source.Copy(copyRoot, nil); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(copyRoot, "workspace")
	data := t.TempDir()
	tofuAt(t, root, data, nil, "init", "-input=false")

	for _, args := range [][]string{
		{"workspace", "new", "production"},
		{"workspace", "select", "-or-create", "production"},
	} {
		if err := source.RequireCommandBackend(
			"workspace",
			"default",
			copyRoot,
			args,
		); err == nil ||
			!strings.Contains(err.Error(), "workspace_dir") {
			t.Fatalf("the workspace transition did not check destination state storage: %v", err)
		}
	}

	if _, err := os.Stat(filepath.Join(data, "environment")); !os.IsNotExist(err) {
		t.Fatal("the rejected workspace transition changed the persistent selection")
	}

	directory := t.TempDir()
	f.replaceFile(
		t,
		"workspace/main.tf",
		fmt.Sprintf(reviewSource(t, "workspace-backend.tf"), state, directory),
	)
	valid, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}

	validCopy := t.TempDir()
	if err := valid.Copy(validCopy, nil); err != nil {
		t.Fatal(err)
	}

	validRoot := filepath.Join(validCopy, "workspace")
	validData := t.TempDir()
	tofuAt(t, validRoot, validData, nil, "init", "-input=false")

	for _, args := range [][]string{
		{"workspace", "new", "production"},
		{"workspace", "select", "default"},
		{"workspace", "select", "production"},
	} {
		if err := valid.RequireCommandBackend("workspace", "default", validCopy, args); err != nil {
			t.Fatal(err)
		}

		tofuAt(t, validRoot, validData, nil, args...)
	}

	selected, err := os.ReadFile(filepath.Join(validData, "environment"))
	if err != nil || strings.TrimSpace(string(selected)) != "production" {
		t.Fatal("the supported transition did not retain the persistent workspace selection")
	}
}

type cachedModules struct {
	Modules []struct {
		Key     string `json:"Key"`
		Source  string `json:"Source"`
		Version string `json:"Version,omitempty"`
		Dir     string `json:"Dir"`
	} `json:"Modules"`
}

func TestModuleCacheCannotRedirectVerifiedLocalModule(t *testing.T) {
	f := newFixture(t)
	state := filepath.Join(t.TempDir(), "state.tfstate")
	f.replaceFile(t, "workspace/main.tf", fmt.Sprintf(reviewSource(t, "local-modules.tf"), state))
	f.replaceFile(t, "workspace/item/main.tf", reviewSource(t, "local-module.tf"))
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}

	if err := source.CheckModuleSources("workspace", "", ""); err != nil {
		t.Fatal(err)
	}

	first := t.TempDir()
	if err := source.Copy(first, nil); err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	tofuAt(t, filepath.Join(first, "workspace"), data, nil, "init", "-input=false")
	second := t.TempDir()
	if err := source.Copy(second, nil); err != nil {
		t.Fatal(err)
	}

	request := installedsource.ModuleCacheRequest{
		Binary:            tofuExecutable(t),
		Workspace:         "workspace",
		CopyRoot:          second,
		DataDirectory:     data,
		WorkspaceSelector: "",
		AllowMissing:      false,
	}
	if err := source.CheckModuleCache(t.Context(), request); err != nil {
		t.Fatalf("normal init followed by planning from a new copy failed: %v", err)
	}

	root := filepath.Join(second, "workspace")
	goodPlan := filepath.Join(t.TempDir(), "verified.tfplan")
	tofuAt(t, root, data, nil, "plan", "-input=false", "-out="+goodPlan)
	good := readReviewedPlan(t, root, data, goodPlan)
	if string(good.PlannedValues.Outputs["module_marker"].Value) != `"verified"` {
		t.Fatal("the normal plan did not evaluate the verified local module")
	}

	outside := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(outside, "main.tf"),
		[]byte(reviewSource(t, "unlisted-module.tf")),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	manifestPath := filepath.Join(data, "modules", "modules.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}

	var manifest cachedModules
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}

	changed := false

	for index := range manifest.Modules {
		if manifest.Modules[index].Key == "item" {
			manifest.Modules[index].Dir = outside
			changed = true
		}
	}

	if !changed {
		t.Fatal("the real module manifest did not contain the expected local module")
	}

	tampered, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(manifestPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}

	controlPlan := filepath.Join(t.TempDir(), "unprotected.tfplan")
	tofuAt(t, root, data, nil, "plan", "-input=false", "-out="+controlPlan)
	control := readReviewedPlan(t, root, data, controlPlan)
	if string(control.PlannedValues.Outputs["module_marker"].Value) != `"unlisted"` {
		t.Fatal("the real OpenTofu control did not reproduce the cached-directory redirect")
	}

	protectedPlan := filepath.Join(t.TempDir(), "protected.tfplan")
	checkErr := source.CheckModuleCache(t.Context(), request)
	if checkErr == nil {
		tofuAt(t, root, data, nil, "plan", "-input=false", "-out="+protectedPlan)
		t.Fatal("the module cache guard accepted an unlisted local module directory")
	}

	if _, err := os.Stat(protectedPlan); !os.IsNotExist(err) {
		t.Fatal("the refused module cache created a plan from unlisted HCL")
	}

	after, err := os.ReadFile(manifestPath)
	if err != nil || string(after) != string(tampered) {
		t.Fatal("the module cache guard rewrote persistent module metadata")
	}
}

func TestNativeModulesUseValidatedConfigsSelector(t *testing.T) {
	f := newFixture(t)
	configuration := "opentofu/render/configuration"
	workspace := "opentofu/suburban/tack"
	f.addFile(
		t,
		filepath.Join(workspace, "main.tf"),
		fmt.Sprintf(
			reviewSource(t, "local-backend.tf"),
			filepath.Join(t.TempDir(), "state.tfstate"),
		),
	)
	f.addFile(t, filepath.Join(configuration, "main.tofu"), reviewSource(t, "native-selector.tofu"))
	f.addFile(
		t,
		"opentofu/render/host-artifact/main.tofu",
		reviewSource(t, "native-host-artifact.tofu"),
	)
	unrelated := "opentofu/modules/unrelated/configuration/main.tf"
	f.addFile(t, unrelated, reviewSource(t, "unrelated-feature.tf"))
	f.addFile(
		t,
		"opentofu/modules/suburban/tack/configuration/main.tf",
		reviewSource(t, "native-locks.tf"),
	)
	encoded, err := json.Marshal(f.document)
	if err != nil {
		t.Fatal(err)
	}

	source, err := installedsource.Verify(
		f.settings,
		f.settings.Root,
		encoded,
		[]string{"opentofu"},
		[]string{"configsctl.yml"},
	)
	if err != nil {
		t.Fatal(err)
	}

	selector := "suburban/tack"
	if err := source.CheckModuleSources(workspace, configuration, selector); err != nil {
		t.Fatalf("the verified Configs selector did not resolve its local module: %v", err)
	}

	copyRoot := t.TempDir()
	if err := source.Copy(copyRoot, nil); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(copyRoot, configuration)
	data := t.TempDir()
	environment := []string{"TF_VAR_workspace=" + selector}
	tofuAt(
		t,
		root,
		data,
		environment,
		"init",
		"-backend=false",
		"-input=false",
		"-lockfile=readonly",
	)
	request := installedsource.ModuleCacheRequest{
		Binary:            tofuExecutable(t),
		Workspace:         configuration,
		CopyRoot:          copyRoot,
		DataDirectory:     data,
		WorkspaceSelector: selector,
		AllowMissing:      false,
	}
	if err := source.CheckModuleCache(t.Context(), request); err != nil {
		t.Fatal(err)
	}

	plan := filepath.Join(t.TempDir(), "native.tfplan")
	tofuAt(
		t,
		root,
		data,
		environment,
		"plan",
		"-refresh=false",
		"-lock=false",
		"-input=false",
		"-out="+plan,
	)
	result := readReviewedPlan(t, root, data, plan)
	var targets struct {
		ModuleHosts   map[string]string `json:"module_hosts"`
		LockEndpoints map[string]struct {
			Local bool `json:"local"`
		} `json:"lock_endpoints"`
	}
	if err := json.Unmarshal(
		result.PlannedValues.Outputs["lock_targets"].Value,
		&targets,
	); err != nil {
		t.Fatal(err)
	}

	if targets.ModuleHosts["module.base"] != "poweredge" ||
		!targets.LockEndpoints["poweredge"].Local {
		t.Fatal(
			"the native selector did not retain the selected module's real local lock ownership",
		)
	}

	if err := source.CheckModuleSources(filepath.Dir(unrelated), "", ""); err == nil {
		t.Fatal("selecting the malformed unrelated feature did not validate its own HCL")
	}

	if err := os.WriteFile(
		filepath.Join(f.settings.Root, unrelated),
		[]byte("changed unrelated bytes"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	if err := source.Copy(t.TempDir(), nil); err == nil {
		t.Fatal("selected-root validation weakened the complete installed-file inventory")
	}
}

func TestModuleResolutionDoesNotRequireStateSecrets(t *testing.T) {
	f := newFixture(t)
	content, err := os.ReadFile(
		filepath.Join("..", "..", "cmd", "configsctl", "testdata", "ci-encrypted-root.tf"),
	)
	if err != nil {
		t.Fatal(err)
	}

	state := filepath.Join(t.TempDir(), "state.tfstate")
	configuration := strings.Replace(
		string(content),
		`backend "local" {}`,
		fmt.Sprintf(reviewSource(t, "backend-block.tf"), state),
		1,
	)
	f.replaceFile(t, "workspace/main.tf", configuration)
	source, err := f.verify(t)
	if err != nil {
		t.Fatal(err)
	}

	copyRoot := t.TempDir()
	if err := source.Copy(copyRoot, nil); err != nil {
		t.Fatal(err)
	}

	request := installedsource.ModuleCacheRequest{
		Binary:            tofuExecutable(t),
		Workspace:         "workspace",
		CopyRoot:          copyRoot,
		DataDirectory:     t.TempDir(),
		WorkspaceSelector: "",
		AllowMissing:      true,
	}
	if err := source.CheckModuleCache(t.Context(), request); err != nil {
		t.Fatalf("nonsecret module resolution required encrypted state or its passphrase: %v", err)
	}
}
