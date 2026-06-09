package redact

import (
	"reflect"
	"sort"
	"testing"
)

func TestAutomatonFindsAllOccurrences(t *testing.T) {
	ac := newAutomaton([][]byte{[]byte("ab"), []byte("bc"), []byte("abVALUE")})
	got := ac.findAll([]byte("xabcabVALUEx"))
	// spans are [start,end): "ab"@1, "bc"@2, "ab"@4, "abVALUE"@4
	want := []span{{1, 3}, {2, 4}, {4, 6}, {4, 11}}
	sort.Slice(got, func(i, j int) bool {
		if got[i].start != got[j].start {
			return got[i].start < got[j].start
		}
		return got[i].end < got[j].end
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findAll = %v, want %v", got, want)
	}
}

func TestAutomatonEmptyPatterns(t *testing.T) {
	ac := newAutomaton(nil)
	if got := ac.findAll([]byte("anything")); len(got) != 0 {
		t.Fatalf("findAll on empty automaton = %v, want none", got)
	}
}
