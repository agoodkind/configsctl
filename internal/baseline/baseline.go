// Package baseline reads, compares, and rewrites the input-default baseline file.
// A baseline records the findings already accepted. The linter fails only on a
// finding whose key is absent from the baseline. Each finding reduces to a key
// that survives a line-number change, so a finding that only moves within a file
// keeps its identity. The file format matches the go-makefile baseline.
package baseline

import (
	"fmt"
	"regexp"
	"strings"
)

// Label is the per-row marker label used in the baseline file.
const Label = "ansible-defaults"

// lineCoordinate matches the first :line: coordinate in a finding. Collapsing it
// to ::: lets a finding match regardless of the line it sits on.
var lineCoordinate = regexp.MustCompile(`:[0-9]+:`)

// Mode is a baseline update mode.
type Mode int

const (
	// ModeSync records the current set and drops fixed rows.
	ModeSync Mode = iota
	// ModePruneFixed keeps only current findings already in the baseline.
	ModePruneFixed
	// ModeAcceptNew records the current set and keeps every old row.
	ModeAcceptNew
)

// modeByName maps each accepted mode string to its Mode.
var modeByName = map[string]Mode{
	"":             ModeSync,
	"sync":         ModeSync,
	"prune-fixed":  ModePruneFixed,
	"remove-fixed": ModePruneFixed,
	"accept-new":   ModeAcceptNew,
}

// ParseMode maps a mode string to a Mode.
func ParseMode(value string) (Mode, error) {
	mode, ok := modeByName[value]
	if !ok {
		return ModeSync, fmt.Errorf("unknown baseline update mode: %q", value)
	}
	return mode, nil
}

// FindingKey reduces a finding to a stable key: strip leading ../, then replace
// the first :line: coordinate with :::.
func FindingKey(finding string) string {
	stripped := finding
	for strings.HasPrefix(stripped, "../") {
		stripped = stripped[len("../"):]
	}
	loc := lineCoordinate.FindStringIndex(stripped)
	if loc == nil {
		return stripped
	}
	return stripped[:loc[0]] + ":::" + stripped[loc[1]:]
}

// Loaded is a parsed baseline file. order is key insertion order; lineByKey maps
// each key to its full row; firstAddedByKey maps each key to its first_added.
type Loaded struct {
	order           []string
	findingByKey    map[string]string
	lineByKey       map[string]string
	firstAddedByKey map[string]string
}

// Keys returns the accepted finding keys.
func (l Loaded) Keys() map[string]struct{} {
	keys := make(map[string]struct{}, len(l.findingByKey))
	for key := range l.findingByKey {
		keys[key] = struct{}{}
	}
	return keys
}

// Load parses baseline body lines, skipping blank and comment lines and
// splitting each row at the `\t# <label>:` marker into finding and metadata.
func Load(lines []string, label string) Loaded {
	loaded := Loaded{
		order:           nil,
		findingByKey:    map[string]string{},
		lineByKey:       map[string]string{},
		firstAddedByKey: map[string]string{},
	}
	marker := "\t# " + label + ":"
	for _, line := range lines {
		if isSkippable(line) {
			continue
		}
		finding, metadata := splitMarker(line, marker)
		if finding == "" {
			continue
		}
		key := FindingKey(finding)
		if _, seen := loaded.findingByKey[key]; !seen {
			loaded.order = append(loaded.order, key)
		}
		loaded.findingByKey[key] = finding
		loaded.lineByKey[key] = line
		loaded.firstAddedByKey[key] = firstAddedFrom(metadata)
	}
	return loaded
}

// Evaluate returns every current finding whose key is absent from the baseline,
// in input order with no deduplication, so each banned line is listed, and the
// count of baseline keys absent from the current findings.
func Evaluate(current []string, baselineKeys map[string]struct{}) ([]string, int) {
	var newFindings []string
	currentKeys := map[string]struct{}{}
	for _, line := range current {
		key := FindingKey(line)
		currentKeys[key] = struct{}{}
		if _, accepted := baselineKeys[key]; accepted {
			continue
		}
		newFindings = append(newFindings, line)
	}
	gone := 0
	for key := range baselineKeys {
		if _, present := currentKeys[key]; !present {
			gone++
		}
	}
	return newFindings, gone
}

// RewriteBody builds the new baseline body for the mode, preserving each row's
// original first_added date.
func RewriteBody(current, old []string, label, now string, mode Mode) []string {
	order, byKey := currentIndex(current)
	prior := Load(old, label)
	render := func(key string) string {
		firstAdded := prior.firstAddedByKey[key]
		if firstAdded == "" {
			firstAdded = now
		}
		return fmt.Sprintf("%s\t# %s:first_added=%s last_seen=%s",
			byKey[key], label, firstAdded, now)
	}
	var body []string
	for _, key := range order {
		if mode == ModePruneFixed {
			if _, kept := prior.findingByKey[key]; !kept {
				continue
			}
		}
		body = append(body, render(key))
	}
	if mode == ModeAcceptNew {
		for _, key := range prior.order {
			if _, present := byKey[key]; !present {
				body = append(body, prior.lineByKey[key])
			}
		}
	}
	return body
}

// Render assembles the baseline file: the generated_at header then the body.
func Render(title, now string, body []string) string {
	lines := append([]string{fmt.Sprintf("# %s: generated_at=%s", title, now)}, body...)
	return strings.Join(lines, "\n") + "\n"
}

func currentIndex(current []string) ([]string, map[string]string) {
	var order []string
	byKey := map[string]string{}
	for _, line := range current {
		if isSkippable(line) {
			continue
		}
		key := FindingKey(line)
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
		}
		byKey[key] = line
	}
	return order, byKey
}

func isSkippable(line string) bool {
	return strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#")
}

func splitMarker(line, marker string) (string, string) {
	before, after, found := strings.Cut(line, marker)
	if !found {
		return line, ""
	}
	return before, after
}

func firstAddedFrom(metadata string) string {
	for token := range strings.FieldsSeq(metadata) {
		if rest, ok := strings.CutPrefix(token, "first_added="); ok {
			return rest
		}
	}
	return ""
}
