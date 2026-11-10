//go:build unix

package installedsource_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/configsctl/internal/installedsource"
	"goodkind.io/configsctl/internal/tofuargs"
)

func selectorSource(
	t *testing.T,
	rootFile string,
	childFile string,
	automatic bool,
) (*installedsource.Source, fixture) {
	t.Helper()
	f := newFixture(t)
	f.addFile(t, "opentofu/render/main.tofu.json", reviewSource(t, rootFile))
	if childFile != "" {
		f.addFile(t, "opentofu/child/main.tofu.json", reviewSource(t, childFile))
	}

	f.addFile(
		t,
		"opentofu/child/approved/main.tofu.json",
		reviewSource(t, "selector-approved.tofu.json"),
	)
	f.addFile(
		t,
		"opentofu/render/approved/main.tofu.json",
		reviewSource(t, "selector-approved.tofu.json"),
	)
	if automatic {
		f.addFile(
			t,
			"opentofu/render/workspace.auto.tfvars.json",
			reviewSource(t, "selector-auto.tfvars.json"),
		)
	}

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

	return source, f
}

func selectorCopy(t *testing.T, source *installedsource.Source) (string, string) {
	t.Helper()
	base := t.TempDir()
	copyRoot := filepath.Join(base, "copy")
	if err := source.Copy(copyRoot, nil); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(
		filepath.Join(outside, "main.tofu.json"),
		[]byte(reviewSource(t, "selector-outside.tofu.json")),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	return copyRoot, filepath.Join(copyRoot, "opentofu", "render")
}

func checkNativeManifest(
	t *testing.T,
	source *installedsource.Source,
	copyRoot string,
	data string,
	selector string,
) error {
	t.Helper()
	request := installedsource.ModuleCacheRequest{
		Binary:            tofuExecutable(t),
		Workspace:         "opentofu/render",
		CopyRoot:          copyRoot,
		DataDirectory:     data,
		WorkspaceSelector: selector,
		AllowMissing:      false,
	}

	return source.CheckModuleCache(t.Context(), request)
}

func TestNativeChildUsesPassedSelectorScope(t *testing.T) {
	source, _ := selectorSource(
		t,
		"selector-root-child.tofu.json",
		"selector-child.tofu.json",
		false,
	)
	if err := source.CheckModuleSources("", "opentofu/render", "approved"); err == nil {
		t.Fatal("the validator replaced the child's passed selector with the root selector")
	}

	copyRoot, root := selectorCopy(t, source)
	data := t.TempDir()
	tofuAt(
		t,
		root,
		data,
		nil,
		tofuargs.WithWorkspaceSelector(
			[]string{"init", "-backend=false", "-input=false", "-lockfile=readonly"},
			"approved",
		)...,
	)
	if err := checkNativeManifest(t, source, copyRoot, data, "approved"); err == nil {
		t.Fatal("the native manifest guard accepted the actual external child directory")
	}

	plan := filepath.Join(t.TempDir(), "unprotected.tfplan")
	tofuAt(
		t,
		root,
		data,
		nil,
		tofuargs.WithWorkspaceSelector(
			[]string{"plan", "-refresh=false", "-lock=false", "-input=false", "-out=" + plan},
			"approved",
		)...,
	)
	if string(
		readReviewedPlan(t, root, data, plan).PlannedValues.Outputs["marker"].Value,
	) != `"outside"` {
		t.Fatal("the unprotected real OpenTofu control did not reproduce the external child marker")
	}
}

func TestNativeSelectorOverridesAutomaticVariableInputs(t *testing.T) {
	source, _ := selectorSource(t, "selector-root-auto.tofu.json", "", true)
	if err := source.CheckModuleSources("", "opentofu/render", "approved"); err != nil {
		t.Fatal(err)
	}

	copyRoot, root := selectorCopy(t, source)
	controlData := t.TempDir()
	environment := []string{"TF_VAR_workspace=approved"}
	tofuAt(
		t,
		root,
		controlData,
		environment,
		"init",
		"-backend=false",
		"-input=false",
		"-lockfile=readonly",
	)
	controlPlan := filepath.Join(t.TempDir(), "unprotected.tfplan")
	tofuAt(
		t,
		root,
		controlData,
		environment,
		"plan",
		"-refresh=false",
		"-lock=false",
		"-input=false",
		"-out="+controlPlan,
	)
	if string(
		readReviewedPlan(t, root, controlData, controlPlan).PlannedValues.Outputs["marker"].Value,
	) != `"outside"` {
		t.Fatal("the real OpenTofu control did not reproduce automatic-variable precedence")
	}

	if err := checkNativeManifest(t, source, copyRoot, controlData, "approved"); err == nil {
		t.Fatal("the actual native manifest did not detect the selector mismatch before plan")
	}

	data := t.TempDir()
	tofuAt(
		t,
		root,
		data,
		environment,
		tofuargs.WithWorkspaceSelector(
			[]string{"init", "-backend=false", "-input=false", "-lockfile=readonly"},
			"approved",
		)...,
	)
	if err := checkNativeManifest(t, source, copyRoot, data, "approved"); err != nil {
		t.Fatal(err)
	}

	plan := filepath.Join(t.TempDir(), "verified.tfplan")
	tofuAt(
		t,
		root,
		data,
		environment,
		tofuargs.WithWorkspaceSelector(
			[]string{"plan", "-refresh=false", "-lock=false", "-input=false", "-out=" + plan},
			"approved",
		)...,
	)
	if string(
		readReviewedPlan(t, root, data, plan).PlannedValues.Outputs["marker"].Value,
	) != `"verified"` {
		t.Fatal("the final native command input did not reserve the approved selector")
	}
}

func TestNativeChildrenUseOwnDefaultsAndConstantInputs(t *testing.T) {
	for _, rootFile := range []string{
		"selector-root-default.tofu.json",
		"selector-root-passed.tofu.json",
	} {
		t.Run(
			rootFile,
			func(t *testing.T) {
				source, _ := selectorSource(t, rootFile, "selector-child-default.tofu.json", false)
				selector := "different-root"
				if err := source.CheckModuleSources("", "opentofu/render", selector); err != nil {
					t.Fatal(err)
				}

				copyRoot, root := selectorCopy(t, source)
				data := t.TempDir()
				tofuAt(
					t,
					root,
					data,
					nil,
					tofuargs.WithWorkspaceSelector(
						[]string{"init", "-backend=false", "-input=false", "-lockfile=readonly"},
						selector,
					)...,
				)
				if err := checkNativeManifest(t, source, copyRoot, data, selector); err != nil {
					t.Fatal(err)
				}

				plan := filepath.Join(t.TempDir(), "child.tfplan")
				tofuAt(
					t,
					root,
					data,
					nil,
					tofuargs.WithWorkspaceSelector(
						[]string{
							"plan",
							"-refresh=false",
							"-lock=false",
							"-input=false",
							"-out=" + plan,
						},
						selector,
					)...,
				)
				if string(
					readReviewedPlan(t, root, data, plan).PlannedValues.Outputs["marker"].Value,
				) != `"verified"` {
					t.Fatal(
						"the child did not retain its own default or explicit constant selector",
					)
				}
			},
		)
	}
}
