package statemigration

import (
	"errors"
	"path/filepath"
)

func (e Engine) currentOriginalConfiguration() (string, error) {
	reviewed, err := filepath.EvalSymlinks(e.Manifest.RestoreConfigurationDirectory)
	if err != nil {
		return "", errors.New("resolve the retained original configuration for restored plan verification")
	}
	configuration := ""
	for _, spec := range e.Manifest.Workspaces {
		if spec.Before.Lineage == "" {
			continue
		}
		original, originalExists := e.OriginalWorkspaces[spec.Selector]
		current, currentExists := e.RestoreWorkspaces[spec.Selector]
		if !originalExists || !currentExists {
			return "", errors.New("restored plan verification requires every original configuration binding")
		}
		selected, err := filepath.EvalSymlinks(original.Directory)
		if err != nil {
			return "", errors.New("resolve the reviewed original workspace binding")
		}
		relative, err := filepath.Rel(reviewed, selected)
		if err != nil || !filepath.IsLocal(relative) {
			return "", errors.New("the original workspace binding must remain inside its reviewed checkout")
		}
		selected, err = filepath.EvalSymlinks(current.Directory)
		if err != nil {
			return "", errors.New("resolve the current original workspace binding")
		}
		root := selected
		for component := relative; component != "."; component = filepath.Dir(component) {
			root = filepath.Dir(root)
		}
		if filepath.Join(root, relative) != selected || (configuration != "" && configuration != root) {
			return "", errors.New("restored plan verification requires one consistent original checkout")
		}
		configuration = root
	}
	if configuration == "" {
		return "", errors.New("restored plan verification lacks its original configuration checkout")
	}
	return configuration, nil
}
