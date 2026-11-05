package tofuargs

import "slices"

// WithWorkspaceSelector binds the reserved native selector after other inputs.
func WithWorkspaceSelector(args []string, selector string) []string {
	result := slices.Clone(args)
	if selector != "" {
		result = append(result, "-var=workspace="+selector)
	}
	return result
}
