package retirement

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"

	"goodkind.io/configsctl/internal/tofufiles"
)

// unchanged skips module resolution when both trees declare the same calls,
// backend configuration and removed addresses. This also covers external
// module sources that neither commit changes.
func unchanged(ctx context.Context, base, head *snapshot) bool {
	dirs := map[string]bool{}

	for dir := range base.files {
		dirs[dir] = true
	}

	for dir := range head.files {
		dirs[dir] = true
	}

	for dir := range dirs {
		baseFiles := activeFiles(base.files[dir])
		headFiles := activeFiles(head.files[dir])
		if maps.Equal(baseFiles, headFiles) {
			continue
		}

		left, understood := comparableModule(ctx, base, dir, len(baseFiles))
		if !understood {
			return false
		}

		right, understood := comparableModule(ctx, head, dir, len(headFiles))
		if !understood {
			return false
		}

		if !maps.Equal(left.removed, right.removed) || !maps.Equal(left.calls, right.calls) ||
			!sameBackend(left.backend, right.backend) {
			return false
		}
	}

	return true
}

func activeFiles(files map[string]treeFile) map[string]treeFile {
	names := map[string]bool{}

	for name := range files {
		names[name] = true
	}

	active := map[string]treeFile{}

	for name, file := range files {
		if tofufiles.Active(name, names) {
			active[name] = file
		}
	}

	return active
}

func comparableModule(ctx context.Context, s *snapshot, dir string, count int) (*module, bool) {
	if count == 0 {
		return &module{}, true
	}

	m, err := s.load(ctx, dir)

	return m, err == nil
}

func sameBackend(left, right *sourceBlock) bool {
	if left == nil || right == nil {
		return left == right
	}

	if !slices.Equal(left.block.Labels, right.block.Labels) {
		return false
	}

	baseFiles, da := left.block.Body.JustAttributes()
	headFiles, db := right.block.Body.JustAttributes()
	if da.HasErrors() || db.HasErrors() || len(baseFiles) != len(headFiles) {
		return false
	}

	for key, attr := range baseFiles {
		other := headFiles[key]
		if other == nil {
			return false
		}

		// Parsing JSON and native syntax can give the same bytes different meanings.
		if strings.HasSuffix(attr.Range.Filename, ".json") == strings.HasSuffix(
			other.Range.Filename,
			".json",
		) &&
			bytes.Equal(
				attr.Expr.Range().SliceBytes(left.data),
				other.Expr.Range().SliceBytes(right.data),
			) {
			continue
		}

		av, ad := attr.Expr.Value(nil)
		bv, bd := other.Expr.Value(nil)
		if ad.HasErrors() || bd.HasErrors() || !av.RawEquals(bv) {
			return false
		}
	}

	return true
}
