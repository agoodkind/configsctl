package main_test

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func providerMirrorTree(t *testing.T) configsTree {
	t.Helper()
	tree := newConfigsTree(t)
	// Directory metadata identifies local sources without a provider process.
	mirror := filepath.Join(tree.root, "mirror \"quoted\" ${literal}")
	if err := os.MkdirAll(filepath.Join(mirror, "tofu.example.test", "example", "local"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := settingsContent + "  provider_mirror: 'mirror \"quoted\" ${literal}'\n"
	writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settings)
	writeFixtureFile(t, filepath.Join(tree.workspaces, "alpha", "main.tf"), `
terraform {
  required_providers {
    terraform = { source = "terraform.io/builtin/terraform" }
  }
}
resource "terraform_data" "example" {
  input = "init-only"
}
`)
	return tree
}

func providerConfigPath(t *testing.T, tree configsTree) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(tree.home, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".tfrc") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("cached provider configurations = %v, want one file", paths)
	}
	return paths[0]
}

func TestTofuProviderMirrorInitAndStableReuse(t *testing.T) {
	requireTofu(t)
	tree := providerMirrorTree(t)
	args := tofuArgs("alpha", "init", "-input=false", "-no-color")
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	path := providerConfigPath(t, tree)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("provider configuration mode = %v, want 0600", info.Mode().Perm())
	}
	for _, directory := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("provider configuration directory mode = %v, want 0700", info.Mode().Perm())
		}
	}
	stamp := time.Unix(1, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	requireSuccess(t, runConfigsctl(t, tree, args...), args...)
	reused, err := os.Stat(providerConfigPath(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, reused) || !reused.ModTime().Equal(stamp) {
		t.Fatal("the second init replaced the cached provider configuration")
	}
	requireNoFile(t, filepath.Join(tree.workspaces, "alpha", "alpha.tfstate"))
}

func TestTofuProviderMirrorRefusesMissingDirectory(t *testing.T) {
	for _, mirror := range []string{"''", "missing", filepath.Join(t.TempDir(), "missing")} {
		t.Run(mirror, func(t *testing.T) {
			tree := newConfigsTree(t)
			writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent+"  provider_mirror: "+mirror+"\n")
			result := runConfigsctl(t, tree, "tofu", "alpha", "version")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "tofu.provider_mirror") {
				t.Fatalf("missing mirror result = %+v, want a provider_mirror error", result)
			}
		})
	}
}

func TestTofuProviderMirrorRefusesUnsafeCachedConfiguration(t *testing.T) {
	requireTofu(t)
	for _, unsafe := range []string{"symlink", "permissions", "content", "directory"} {
		t.Run(unsafe, func(t *testing.T) {
			tree := providerMirrorTree(t)
			args := tofuArgs("alpha", "init", "-input=false", "-no-color")
			requireSuccess(t, runConfigsctl(t, tree, args...), args...)
			path := providerConfigPath(t, tree)
			switch unsafe {
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(tree.root, "untouched.tfrc")
				writeFixtureFile(t, target, "unchanged\n")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "content":
				if err := os.WriteFile(path, []byte("changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			result := runConfigsctl(t, tree, "tofu", "alpha", "version")
			if result.exitCode == 0 || !strings.Contains(result.stderr, "provider configuration") {
				t.Fatalf("unsafe configuration result = %+v, want refusal", result)
			}
		})
	}
}

func TestTofuProviderMirrorPreservesNativeConfigurationWhenAbsent(t *testing.T) {
	requireTofu(t)
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "mirror"}[configured], func(t *testing.T) {
			tree := providerMirrorTree(t)
			if !configured {
				writeFixtureFile(t, filepath.Join(tree.root, "configsctl.yml"), settingsContent)
			}
			configuration := filepath.Join(tree.root, "invalid.tfrc")
			writeFixtureFile(t, configuration, "provider_installation {\n")
			command := exec.CommandContext(context.Background(), configsctlBinary, "tofu", "alpha", "init", "-input=false", "-no-color")
			command.Dir = tree.root
			command.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"HOME=" + tree.home,
				"TF_CLI_CONFIG_FILE=" + configuration,
			}
			output, err := command.CombinedOutput()
			if configured {
				if err != nil || strings.Contains(string(output), "Error parsing "+configuration) {
					t.Fatalf("the declared mirror did not override native CLI configuration: %v\n%s", err, output)
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "Error parsing "+configuration) {
				t.Fatalf("the command did not preserve the native CLI configuration: %v\n%s", err, output)
			}
		})
	}
}
