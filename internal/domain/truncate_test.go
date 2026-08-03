package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateContent(t *testing.T) {
	cases := []struct {
		name          string
		content       string
		maxChars      int
		startChar     int
		wantText      string
		wantTruncated bool
		wantTotal     int
		wantNext      int
	}{
		{
			name:    "shorter than cap returns everything unmarked",
			content: "hello world", maxChars: 100,
			wantText: "hello world", wantTotal: 11, wantNext: 11,
		},
		{
			name:    "exactly at cap is not truncated",
			content: "hello", maxChars: 5,
			wantText: "hello", wantTotal: 5, wantNext: 5,
		},
		{
			name:    "final chunk gets no marker",
			content: "abcdefghij", maxChars: 5, startChar: 5,
			wantText: "fghij", wantTotal: 10, wantNext: 10,
		},
		{
			name:    "zero maxChars means unlimited",
			content: "abcdefghij", maxChars: 0,
			wantText: "abcdefghij", wantTotal: 10, wantNext: 10,
		},
		{
			name:    "unlimited still honors startChar",
			content: "abcdefghij", maxChars: 0, startChar: 7,
			wantText: "hij", wantTotal: 10, wantNext: 10,
		},
		{
			name:    "offset past the end explains itself",
			content: "abcdefghij", maxChars: 5, startChar: 10,
			wantText:  "[omnifeed: no content at offset 10 — total 10 characters]",
			wantTotal: 10, wantNext: 10,
		},
		{
			name:    "offset far past the end explains itself",
			content: "abc", maxChars: 5, startChar: 999,
			wantText:  "[omnifeed: no content at offset 999 — total 3 characters]",
			wantTotal: 3, wantNext: 3,
		},
		{
			name:    "empty content at offset 0 stays empty",
			content: "", maxChars: 10,
			wantText: "", wantTotal: 0, wantNext: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateContent(tc.content, tc.maxChars, tc.startChar)
			if got.Text != tc.wantText {
				t.Errorf("Text:\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Truncated() != tc.wantTruncated {
				t.Errorf("Truncated: got %v, want %v", got.Truncated(), tc.wantTruncated)
			}
			if got.TotalChars != tc.wantTotal {
				t.Errorf("TotalChars: got %d, want %d", got.TotalChars, tc.wantTotal)
			}
			if got.NextStartChar != tc.wantNext {
				t.Errorf("NextStartChar: got %d, want %d", got.NextStartChar, tc.wantNext)
			}
		})
	}
}

// A truncated window must (a) never exceed maxChars runes marker included — the
// caller advertises maxChars as a hard ceiling (anthropic/maxResultSizeChars) —
// (b) end with a marker naming the resume offset, and (c) resume exactly where
// the kept content stopped.
func TestTruncateContent_MarkerWithinBudget(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		maxChars  int
		startChar int
	}{
		{name: "ascii", content: strings.Repeat("abcdefghij", 100), maxChars: 200},
		{name: "from an offset", content: strings.Repeat("abcdefghij", 100), maxChars: 150, startChar: 400},
		{name: "multibyte", content: strings.Repeat("héllo wörld — ünïcode ", 50), maxChars: 200},
		{name: "emoji", content: strings.Repeat("ab🎉cd", 100), maxChars: 150},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateContent(tc.content, tc.maxChars, tc.startChar)
			if !got.Truncated() {
				t.Fatal("expected a truncated result")
			}
			if n := utf8.RuneCountInString(got.Text); n > tc.maxChars {
				t.Errorf("Text is %d runes, exceeds maxChars %d", n, tc.maxChars)
			}
			i := strings.Index(got.Text, "\n\n[omnifeed: content truncated")
			if i < 0 {
				t.Fatalf("Text has no truncation marker:\n%q", got.Text)
			}
			kept := utf8.RuneCountInString(got.Text[:i])
			if want := got.NextStartChar - tc.startChar; kept != want {
				t.Errorf("kept %d runes, NextStartChar implies %d", kept, want)
			}
		})
	}
}

// Chaining start_char from each marker must reproduce the source exactly, with
// no character lost or repeated at the seams.
func TestTruncateContent_ChunksReassemble(t *testing.T) {
	content := strings.Repeat("héllo wörld ", 50) // 600 runes, 700+ bytes
	var b strings.Builder
	for offset := 0; ; {
		got := TruncateContent(content, 150, offset)
		chunk := got.Text
		if i := strings.Index(chunk, "\n\n[omnifeed:"); i >= 0 {
			chunk = chunk[:i]
		}
		b.WriteString(chunk)
		if !got.Truncated() {
			break
		}
		offset = got.NextStartChar
	}
	if b.String() != content {
		t.Errorf("reassembled content differs from source:\n got %q\nwant %q", b.String(), content)
	}
}

func TestTruncatableContentType(t *testing.T) {
	cases := map[string]bool{
		ContentTypeMarkdown: true,
		ContentTypeTOON:     false,
		"json":              false,
		"":                  false,
		"html":              false,
	}
	for contentType, want := range cases {
		if got := TruncatableContentType(contentType); got != want {
			t.Errorf("TruncatableContentType(%q): got %v, want %v", contentType, got, want)
		}
	}
}
