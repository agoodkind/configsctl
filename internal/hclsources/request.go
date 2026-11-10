// Package hclsources checks native OpenTofu files before an artifact build.
package hclsources

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"strings"

	"goodkind.io/configsctl/internal/tofufiles"
)

type request struct {
	Files []sourceFile `json:"files"`
}

type sourceFile struct {
	Source      string  `json:"source"`
	Destination string  `json:"destination"`
	Content     *string `json:"content"`
}

// CheckRequest validates the supplied JSON without opening the source or installed files.
func CheckRequest(data []byte) error {
	var req request
	if err := json.Unmarshal(data, &req, json.RejectUnknownMembers(true)); err != nil {
		return errors.New("supply valid UTF-8 request JSON with unique fields and the required field types")
	}
	if len(req.Files) == 0 {
		return errors.New("supply a nonempty files array")
	}
	return validate(req.Files)
}

func (f sourceFile) checkFields(index int) error {
	if f.Source == "" || path.Clean(f.Source) != f.Source || !nativeName(f.Source) {
		return fmt.Errorf("files[%d]: supply a canonical native OpenTofu source filename", index)
	}
	if !path.IsAbs(f.Destination) || path.Clean(f.Destination) != f.Destination || !nativeName(f.Destination) {
		return fmt.Errorf("files[%d]: supply an absolute native OpenTofu destination filename", index)
	}
	if f.Content == nil {
		return fmt.Errorf("files[%d]: supply the exact source content as a string", index)
	}
	return nil
}

func nativeName(filename string) bool {
	return strings.HasSuffix(filename, ".tf") || strings.HasSuffix(filename, ".tofu")
}

func validate(files []sourceFile) error {
	directories := make(map[string]map[string]bool)
	for i, file := range files {
		if err := file.checkFields(i); err != nil {
			return err
		}
		dir, name := path.Dir(file.Destination), path.Base(file.Destination)
		if directories[dir] == nil {
			directories[dir] = make(map[string]bool)
		}
		if directories[dir][name] {
			return fmt.Errorf("files[%d]: supply each installed filename once", i)
		}
		directories[dir][name] = true
	}

	declarations := make(map[installedVariable]string)
	parsed := make([]*source, 0, len(files))
	for _, file := range files {
		src, err := parse(file)
		if err != nil {
			return err
		}
		active := tofufiles.Active(path.Base(file.Destination), directories[path.Dir(file.Destination)])
		if err := src.declare(declarations, active); err != nil {
			return err
		}
		parsed = append(parsed, src)
	}
	for _, src := range parsed {
		if err := src.check(declarations); err != nil {
			return err
		}
	}
	return nil
}
