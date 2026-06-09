package redact

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sort"
)

// MinLen is the shortest secret value that can be redacted safely. A non-empty
// value shorter than this would risk masking unrelated output, so the caller
// fails closed when one is present.
const MinLen = 16

// placeholderPrefix and placeholderSuffix wrap the matched key in the emitted
// placeholder, as <redacted:KEY>.
const (
	placeholderPrefix = "<redacted:"
	placeholderSuffix = ">"
)

// Pattern is one secret value and the vault key whose value it is. Label is not
// secret; Value is.
type Pattern struct {
	Value []byte
	Label string
}

// labeledSpan is a covered byte range carrying the label to emit for it.
type labeledSpan struct {
	span  span
	label string
}

// Validate returns the label of the first pattern whose value is non-empty and
// shorter than MinLen, with ok=false. Empty values are ignored (they match
// nothing). ok=true means every value is safe to redact.
func Validate(patterns []Pattern) (badKey string, ok bool) {
	for _, p := range patterns {
		if len(p.Value) > 0 && len(p.Value) < MinLen {
			return p.Label, false
		}
	}
	return "", true
}

// mergeSpans coalesces overlapping spans (those sharing at least one byte) into
// single covered regions. Touching spans (end == next start) are left separate,
// because distinct adjacent secrets share no bytes and each keeps its own label.
// Input order is arbitrary; output is sorted by start and non-overlapping. Each
// merged region keeps the label of its earliest-starting member (ties broken by
// lexicographically smaller label) so the placeholder is deterministic. The
// merge is what makes overlapping secrets leak-proof: when one secret's bytes sit
// inside another's span, the two become one redaction and no fragment survives.
func mergeSpans(in []labeledSpan) []labeledSpan {
	if len(in) == 0 {
		return nil
	}
	s := append([]labeledSpan(nil), in...)
	sort.Slice(s, func(i, j int) bool {
		if s[i].span.start != s[j].span.start {
			return s[i].span.start < s[j].span.start
		}
		if s[i].span.end != s[j].span.end {
			return s[i].span.end < s[j].span.end
		}
		return s[i].label < s[j].label
	})
	out := []labeledSpan{s[0]}
	for _, cur := range s[1:] {
		last := &out[len(out)-1]
		if cur.span.start < last.span.end {
			if cur.span.end > last.span.end {
				last.span.end = cur.span.end
			}
			continue
		}
		out = append(out, cur)
	}
	return out
}

// Writer wraps an [io.Writer] and rewrites every secret occurrence to
// <redacted:KEY>. It is streaming-safe: a secret split across Write calls is
// still caught, because the trailing maxLen-1 bytes are held back until enough
// input arrives or Close is called. A Writer is not safe for concurrent use by
// multiple goroutines; the caller serializes writes.
type Writer struct {
	dst      io.Writer
	ac       *automaton
	patterns []Pattern
	buf      []byte
	maxLen   int
}

// New returns a Writer over dst. An empty pattern set makes the Writer a
// transparent passthrough.
func New(dst io.Writer, patterns []Pattern) *Writer {
	values := make([][]byte, 0, len(patterns))
	maxLen := 0
	for _, p := range patterns {
		values = append(values, p.Value)
		if len(p.Value) > maxLen {
			maxLen = len(p.Value)
		}
	}
	return &Writer{
		dst:      dst,
		ac:       newAutomaton(values),
		patterns: patterns,
		buf:      nil,
		maxLen:   maxLen,
	}
}

// Write buffers p, then emits the longest prefix of the buffer that no
// in-progress match can still extend, holding the rest back. A secret that
// starts in the buffer but might continue in a later write is never emitted
// split, because the emit boundary is pulled back before any matched span that
// crosses it.
func (w *Writer) Write(p []byte) (int, error) {
	if w.maxLen == 0 {
		if _, err := w.dst.Write(p); err != nil {
			return 0, fmt.Errorf("redact passthrough write: %w", err)
		}
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	hold := w.maxLen - 1
	if len(w.buf) <= hold {
		return len(p), nil
	}
	merged := w.mergedSpans(w.buf)
	emitEnd := len(w.buf) - hold
	for _, ls := range merged {
		if ls.span.start < emitEnd && ls.span.end > emitEnd {
			emitEnd = ls.span.start
		}
	}
	if emitEnd <= 0 {
		return len(p), nil
	}
	out := render(w.buf[:emitEnd], spansBefore(merged, emitEnd))
	if _, err := w.dst.Write(out); err != nil {
		return 0, fmt.Errorf("redact write: %w", err)
	}
	w.buf = append(w.buf[:0], w.buf[emitEnd:]...)
	return len(p), nil
}

// Close redacts and emits any held-back tail.
func (w *Writer) Close() error {
	if len(w.buf) == 0 {
		return nil
	}
	out := render(w.buf, w.mergedSpans(w.buf))
	w.buf = nil
	if _, err := w.dst.Write(out); err != nil {
		slog.Error("redact close write failed", "err", err)
		return fmt.Errorf("redact close write: %w", err)
	}
	return nil
}

// mergedSpans finds every occurrence in data, labels each, and merges
// overlapping or touching spans into non-overlapping labeled regions.
func (w *Writer) mergedSpans(data []byte) []labeledSpan {
	rawSpans := w.ac.findAll(data)
	if len(rawSpans) == 0 {
		return nil
	}
	labeled := make([]labeledSpan, 0, len(rawSpans))
	for _, s := range rawSpans {
		labeled = append(labeled, labeledSpan{span: s, label: w.labelFor(data, s)})
	}
	return mergeSpans(labeled)
}

// spansBefore returns the merged spans that end at or before bound. The input is
// sorted by start, so this is a prefix of it.
func spansBefore(merged []labeledSpan, bound int) []labeledSpan {
	out := merged[:0:0]
	for _, ls := range merged {
		if ls.span.end <= bound {
			out = append(out, ls)
		}
	}
	return out
}

// render rewrites each covered region of data to its placeholder. spans must be
// sorted by start, non-overlapping, and fully within data.
func render(data []byte, spans []labeledSpan) []byte {
	if len(spans) == 0 {
		return data
	}
	var out bytes.Buffer
	prev := 0
	for _, ls := range spans {
		out.Write(data[prev:ls.span.start])
		out.WriteString(placeholderPrefix + ls.label + placeholderSuffix)
		prev = ls.span.end
	}
	out.Write(data[prev:])
	return out.Bytes()
}

// labelFor returns the vault key whose value produced span s. The automaton
// reports span lengths but not which pattern; match by (length, content)
// against the configured patterns, choosing the lexicographically smallest key
// on a tie so the placeholder is deterministic.
func (w *Writer) labelFor(data []byte, s span) string {
	value := data[s.start:s.end]
	best := ""
	for _, p := range w.patterns {
		if len(p.Value) == len(value) && bytes.Equal(p.Value, value) {
			if best == "" || p.Label < best {
				best = p.Label
			}
		}
	}
	return best
}
