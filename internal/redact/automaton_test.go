package redact

import (
	"reflect"
	"sort"
	"testing"
)

func TestAutomatonFindsAllOccurrences(t *testing.T) {
	ac := newAutomaton([][]byte{[]byte("ab"), []byte("bc"), []byte("abVALUE")})
	got, _ := ac.scan([]byte("xabcabVALUEx"))
	// spans are [start,end): "ab"@1, "bc"@2, "ab"@4, "abVALUE"@4
	want := []span{{1, 3}, {2, 4}, {4, 6}, {4, 11}}
	sort.Slice(got, func(i, j int) bool {
		if got[i].start != got[j].start {
			return got[i].start < got[j].start
		}
		return got[i].end < got[j].end
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scan spans = %v, want %v", got, want)
	}
}

func TestAutomatonEmptyPatterns(t *testing.T) {
	ac := newAutomaton(nil)
	got, partial := ac.scan([]byte("anything"))
	if len(got) != 0 {
		t.Fatalf("scan on empty automaton = %v, want none", got)
	}
	if partial != 0 {
		t.Fatalf("partial = %d, want 0", partial)
	}
}

// TestAutomatonPartialLength pins the streaming hold-back length: scan reports
// how many trailing bytes could still begin a match that continues past the end
// of the input, so the writer withholds those bytes and nothing else.
func TestAutomatonPartialLength(t *testing.T) {
	ac := newAutomaton([][]byte{[]byte("abcdef"), []byte("xyz")})
	tests := []struct {
		name string
		data string
		want int
	}{
		{name: "no pattern prefix at the end", data: "hello world", want: 0},
		{name: "ends mid pattern", data: "log line abc", want: 3},
		{name: "ends on a complete match", data: "abcdef", want: 6},
		{name: "ends on the other pattern's first byte", data: "done x", want: 1},
		{name: "empty input", data: "", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := ac.scan([]byte(tc.data)); got != tc.want {
				t.Fatalf("scan(%q) partial = %d, want %d", tc.data, got, tc.want)
			}
		})
	}
}
