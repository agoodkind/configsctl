package lint

import (
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
)

// excludedDirs are directory names never scanned for Ansible input config.
var excludedDirs = map[string]struct{}{
	".git": {}, "node_modules": {}, "go": {}, ".make": {},
}

// ansibleTree is the subtree whose YAML is Ansible input config. Templates are
// scanned repo-wide because a .j2 is always an Ansible template here.
const ansibleTree = "ansible"

// Discover returns the candidate files under root: every .j2 template repo-wide,
// plus the .yml and .yaml files under the ansible tree, excluding generated and
// vendored directories.
func Discover(root string) []string {
	seen := map[string]struct{}{}
	walkTree(root, func(path string) {
		if strings.HasSuffix(path, ".j2") {
			seen[path] = struct{}{}
		}
	})
	walkTree(filepath.Join(root, ansibleTree), func(path string) {
		if strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml") {
			seen[path] = struct{}{}
		}
	})
	files := make([]string, 0, len(seen))
	for file := range seen {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

// walkTree visits every file under root, skipping excluded directories. A
// missing or unreadable root is logged and yields no files.
func walkTree(root string, visit func(path string)) {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if _, excluded := excludedDirs[entry.Name()]; excluded {
				return filepath.SkipDir
			}
			return nil
		}
		visit(path)
		return nil
	})
	if err != nil {
		slog.Warn("skip unwalkable tree", "root", root, "err", err)
	}
}
