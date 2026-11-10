package redact

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []Pattern{{Value: []byte("0123456789abcdef"), Label: "vault_ok"}} // 16
	if key, valid := Validate(ok); !valid {
		t.Fatalf("Validate(16-char) = (%q,false), want valid", key)
	}
	short := []Pattern{{Value: []byte("short"), Label: "vault_bad"}}
	key, valid := Validate(short)
	if valid || key != "vault_bad" {
		t.Fatalf("Validate(short) = (%q,%v), want (vault_bad,false)", key, valid)
	}
	empty := []Pattern{{Value: []byte(""), Label: "vault_empty"}}
	if key, valid := Validate(empty); !valid {
		t.Fatalf("Validate(empty value) = (%q,false), want valid (empty ignored)", key)
	}
}

func TestMergeSpans(t *testing.T) {
	in := []labeledSpan{
		{span{1, 3}, "a"}, {span{2, 4}, "b"}, {span{4, 6}, "c"}, {span{8, 9}, "d"},
	}
	// [1,3)&[2,4) overlap (2<3) -> [1,4); [4,6) only touches end 4, stays separate;
	// [8,9) separate.
	got := mergeSpans(in)
	want := []labeledSpan{{span{1, 4}, "a"}, {span{4, 6}, "c"}, {span{8, 9}, "d"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeSpans = %v, want %v", got, want)
	}
}

func redactAll(t *testing.T, patterns []Pattern, chunks ...string) string {
	t.Helper()
	var sb strings.Builder
	w := New(&sb, patterns)
	for _, c := range chunks {
		if _, err := w.Write([]byte(c)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return sb.String()
}

func TestWriterRedactsSingle(t *testing.T) {
	pats := []Pattern{{Value: []byte("supersecretvalue1234"), Label: "vault_token"}}
	got := redactAll(t, pats, "x=supersecretvalue1234;")
	if got != "x=<redacted:vault_token>;" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterRedactsAcrossChunks(t *testing.T) {
	pats := []Pattern{{Value: []byte("supersecretvalue1234"), Label: "vault_token"}}
	got := redactAll(t, pats, "x=supersecret", "value1234;")
	if got != "x=<redacted:vault_token>;" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterOverlapNoLeak(t *testing.T) {
	pats := []Pattern{
		{Value: []byte("SECRETabcdefghij"), Label: "vault_a"}, // 16
		{Value: []byte("abcdefghijVALUE1"), Label: "vault_b"}, // 16, shares abcdefghij
	}
	got := redactAll(t, pats, "xSECRETabcdefghijVALUE1x")
	if strings.Contains(got, "VALUE1") || strings.Contains(got, "SECRET") {
		t.Fatalf("overlap leaked a secret fragment: %q", got)
	}
}

func TestWriterEmptyPatternsPassthrough(t *testing.T) {
	got := redactAll(t, nil, "nothing to hide here")
	if got != "nothing to hide here" {
		t.Fatalf("got %q", got)
	}
}

func TestWriterByteAtATime(t *testing.T) {
	pats := []Pattern{{Value: []byte("supersecretvalue1234"), Label: "vault_token"}}
	chunks := make([]string, 0, len("a=supersecretvalue1234;b"))
	for _, b := range []byte("a=supersecretvalue1234;b") {
		chunks = append(chunks, string([]byte{b}))
	}
	got := redactAll(t, pats, chunks...)
	if got != "a=<redacted:vault_token>;b" {
		t.Fatalf("got %q", got)
	}
}

// TestWriterEmitsNonMatchingTextImmediately pins the streaming property the
// deploy log depends on: text that ends in nothing a secret could continue from
// reaches the destination on the Write that produced it, however long the
// longest secret is. Without it a reader tailing the log lags by maxLen-1 bytes,
// which for this repo's vault is several thousand.
func TestWriterEmitsNonMatchingTextImmediately(t *testing.T) {
	long := bytes.Repeat([]byte("S"), 4096)
	pats := []Pattern{{Value: long, Label: "vault_long"}}
	var sb strings.Builder
	w := New(&sb, pats)

	line := "TASK [install packages] ********\n"
	if _, err := w.Write([]byte(line)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if sb.String() != line {
		t.Fatalf("held back non-matching text: got %q, want %q", sb.String(), line)
	}
}

// TestWriterHoldsBackOnlyThePartialMatch pins the other half: when a write ends
// part-way into a secret, exactly that partial tail is withheld, the text before
// it is emitted, and the secret is still redacted whole once the rest arrives.
func TestWriterHoldsBackOnlyThePartialMatch(t *testing.T) {
	secret := "supersecretvalue1234"
	pats := []Pattern{{Value: []byte(secret), Label: "vault_token"}}
	var sb strings.Builder
	w := New(&sb, pats)

	head := secret[:8]
	if _, err := w.Write([]byte("ok line\n" + head)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := sb.String(); got != "ok line\n" {
		t.Fatalf("after partial secret, dst = %q, want %q", got, "ok line\n")
	}
	if _, err := w.Write([]byte(secret[8:] + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := "ok line\n<redacted:vault_token>\n"
	if got := sb.String(); got != want {
		t.Fatalf("dst = %q, want %q", got, want)
	}
	if strings.Contains(sb.String(), head) {
		t.Fatalf("leaked a secret fragment: %q", sb.String())
	}
}

func TestWriterAdjacentSecrets(t *testing.T) {
	pats := []Pattern{
		{Value: []byte("AAAAAAAAAAAAAAAA"), Label: "vault_a"}, // 16
		{Value: []byte("BBBBBBBBBBBBBBBB"), Label: "vault_b"}, // 16
	}
	got := redactAll(t, pats, "xAAAAAAAAAAAAAAAABBBBBBBBBBBBBBBBy")
	if got != "x<redacted:vault_a><redacted:vault_b>y" {
		t.Fatalf("got %q", got)
	}
}
