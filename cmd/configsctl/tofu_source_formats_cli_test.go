package main_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTofuLoadsPreferredSourceFormats(t *testing.T) {
	for _, format := range []string{"native", "json"} {
		t.Run(format, func(t *testing.T) {
			requireTofu(t)
			tree := newConfigsTree(t)
			root := filepath.Join(tree.workspaces, "alpha")
			if format == "native" {
				for _, name := range []string{"backend", "main"} {
					if err := os.Rename(filepath.Join(root, name+".tf"), filepath.Join(root, name+".tofu")); err != nil {
						t.Fatal(err)
					}
					writeFixtureFile(t, filepath.Join(root, name+".tf"), "invalid ignored source")
				}
			} else {
				for _, name := range []string{"backend", "main"} {
					if err := os.Remove(filepath.Join(root, name+".tf")); err != nil {
						t.Fatal(err)
					}
					writeFixtureFile(t, filepath.Join(root, name+".tf.json"), "invalid ignored JSON")
				}
				for _, name := range []string{"backend", "main"} {
					data, err := os.ReadFile(filepath.Join("testdata", "sourceformats", name+".tofu.json"))
					if err != nil {
						t.Fatal(err)
					}
					writeFixtureFile(t, filepath.Join(root, name+".tofu.json"), string(data))
				}
			}
			for _, command := range []string{"init", "plan"} {
				args := tofuArgs("alpha", command, "-input=false")
				requireSuccess(t, runConfigsctl(t, tree, args...), args...)
			}
		})
	}
}
