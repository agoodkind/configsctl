// Package redact replaces secret byte sequences in a stream with a labeled
// placeholder, so a command's stdout and stderr never expose secret values.
package redact

// span is a half-open byte range [start,end) covered by a secret occurrence.
type span struct {
	start int
	end   int
}

// automaton is an Aho-Corasick matcher over a fixed set of patterns. It finds
// every occurrence of every pattern in a single pass over the input.
type automaton struct {
	next   []map[byte]int // goto per state
	fail   []int          // failure links
	out    [][]int        // pattern lengths ending at each state
	maxLen int
}

// newAutomaton builds the matcher from pattern byte slices. Empty or nil input
// yields a matcher that finds nothing.
func newAutomaton(patterns [][]byte) *automaton {
	ac := &automaton{
		next:   []map[byte]int{{}},
		fail:   []int{0},
		out:    [][]int{nil},
		maxLen: 0,
	}
	for _, p := range patterns {
		if len(p) == 0 {
			continue
		}
		ac.add(p)
		if len(p) > ac.maxLen {
			ac.maxLen = len(p)
		}
	}
	ac.buildFailureLinks()
	return ac
}

func (ac *automaton) add(p []byte) {
	state := 0
	for _, b := range p {
		nxt, ok := ac.next[state][b]
		if !ok {
			nxt = len(ac.next)
			ac.next = append(ac.next, map[byte]int{})
			ac.fail = append(ac.fail, 0)
			ac.out = append(ac.out, nil)
			ac.next[state][b] = nxt
		}
		state = nxt
	}
	ac.out[state] = append(ac.out[state], len(p))
}

func (ac *automaton) buildFailureLinks() {
	queue := make([]int, 0, len(ac.next))
	for _, s := range ac.next[0] {
		ac.fail[s] = 0
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for b, nxt := range ac.next[state] {
			queue = append(queue, nxt)
			f := ac.fail[state]
			for f != 0 {
				if _, ok := ac.next[f][b]; ok {
					break
				}
				f = ac.fail[f]
			}
			if t, ok := ac.next[f][b]; ok && t != nxt {
				ac.fail[nxt] = t
			} else {
				ac.fail[nxt] = 0
			}
			ac.out[nxt] = append(ac.out[nxt], ac.out[ac.fail[nxt]]...)
		}
	}
}

// step advances the automaton from state on byte b, following failure links.
func (ac *automaton) step(state int, b byte) int {
	for {
		if nxt, ok := ac.next[state][b]; ok {
			return nxt
		}
		if state == 0 {
			return 0
		}
		state = ac.fail[state]
	}
}

// findAll returns every occurrence span over data, scanning from state 0.
func (ac *automaton) findAll(data []byte) []span {
	var spans []span
	state := 0
	for i := range data {
		state = ac.step(state, data[i])
		for _, length := range ac.out[state] {
			spans = append(spans, span{start: i - length + 1, end: i + 1})
		}
	}
	return spans
}
