package redact

import (
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
