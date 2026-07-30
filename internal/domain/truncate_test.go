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
		wantReturned  int
		wantNext      int
	}{
		{
			name:    "shorter than cap returns everything unmarked",
			content: "hello world", maxChars: 100,
			wantText: "hello world", wantTotal: 11, wantReturned: 11, wantNext: 11,
		},
		{
			name:    "exactly at cap is not truncated",
			content: "hello", maxChars: 5,
			wantText: "hello", wantTotal: 5, wantReturned: 5, wantNext: 5,
		},
		{
			name:    "over cap truncates and appends the marker",
			content: "abcdefghij", maxChars: 4,
			wantText: "abcd\n\n[omnifeed: content truncated at 4 of 10 characters. " +
				"Call fetch_url again with start_char=4 to continue.]",
			wantTruncated: true, wantTotal: 10, wantReturned: 4, wantNext: 4,
		},
		{
			name:    "continuation from an offset reports the next offset",
			content: "abcdefghij", maxChars: 3, startChar: 4,
			wantText: "efg\n\n[omnifeed: content truncated at 7 of 10 characters. " +
				"Call fetch_url again with start_char=7 to continue.]",
			wantTruncated: true, wantTotal: 10, wantReturned: 3, wantNext: 7,
		},
		{
			name:    "final chunk gets no marker",
			content: "abcdefghij", maxChars: 5, startChar: 5,
			wantText: "fghij", wantTotal: 10, wantReturned: 5, wantNext: 10,
		},
		{
			name:    "zero maxChars means unlimited",
			content: "abcdefghij", maxChars: 0,
			wantText: "abcdefghij", wantTotal: 10, wantReturned: 10, wantNext: 10,
		},
		{
			name:    "unlimited still honors startChar",
			content: "abcdefghij", maxChars: 0, startChar: 7,
			wantText: "hij", wantTotal: 10, wantReturned: 3, wantNext: 10,
		},
		{
			name:    "offset past the end explains itself",
			content: "abcdefghij", maxChars: 5, startChar: 10,
			wantText:  "[omnifeed: no content at offset 10 — total 10 characters]",
			wantTotal: 10, wantReturned: 0, wantNext: 10,
		},
		{
			name:    "offset far past the end explains itself",
			content: "abc", maxChars: 5, startChar: 999,
			wantText:  "[omnifeed: no content at offset 999 — total 3 characters]",
			wantTotal: 3, wantReturned: 0, wantNext: 3,
		},
		{
			// Counting bytes here would slice mid-character and produce U+FFFD.
			name:    "multibyte content is counted and cut by rune",
			content: "héllo wörld — ünïcode", maxChars: 5,
			wantText: "héllo\n\n[omnifeed: content truncated at 5 of 21 characters. " +
				"Call fetch_url again with start_char=5 to continue.]",
			wantTruncated: true, wantTotal: 21, wantReturned: 5, wantNext: 5,
		},
		{
			name:    "multibyte startChar resumes on a rune boundary",
			content: "héllo wörld", maxChars: 4, startChar: 6,
			wantText: "wörl\n\n[omnifeed: content truncated at 10 of 11 characters. " +
				"Call fetch_url again with start_char=10 to continue.]",
			wantTruncated: true, wantTotal: 11, wantReturned: 4, wantNext: 10,
		},
		{
			name:    "emoji are single characters",
			content: "ab🎉cd", maxChars: 3,
			wantText: "ab🎉\n\n[omnifeed: content truncated at 3 of 5 characters. " +
				"Call fetch_url again with start_char=3 to continue.]",
			wantTruncated: true, wantTotal: 5, wantReturned: 3, wantNext: 3,
		},
		{
			name:    "negative startChar is clamped to 0",
			content: "abc", maxChars: 2, startChar: -5,
			wantText: "ab\n\n[omnifeed: content truncated at 2 of 3 characters. " +
				"Call fetch_url again with start_char=2 to continue.]",
			wantTruncated: true, wantTotal: 3, wantReturned: 2, wantNext: 2,
		},
		{
			name:    "empty content at offset 0 stays empty",
			content: "", maxChars: 10,
			wantText: "", wantTotal: 0, wantReturned: 0, wantNext: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateContent(tc.content, tc.maxChars, tc.startChar)
			if got.Text != tc.wantText {
				t.Errorf("Text:\n got %q\nwant %q", got.Text, tc.wantText)
			}
			if got.Truncated != tc.wantTruncated {
				t.Errorf("Truncated: got %v, want %v", got.Truncated, tc.wantTruncated)
			}
			if got.TotalChars != tc.wantTotal {
				t.Errorf("TotalChars: got %d, want %d", got.TotalChars, tc.wantTotal)
			}
			if got.ReturnedChars != tc.wantReturned {
				t.Errorf("ReturnedChars: got %d, want %d", got.ReturnedChars, tc.wantReturned)
			}
			if got.NextStartChar != tc.wantNext {
				t.Errorf("NextStartChar: got %d, want %d", got.NextStartChar, tc.wantNext)
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
		got := TruncateContent(content, 37, offset)
		chunk := got.Text
		if i := strings.Index(chunk, "\n\n[omnifeed:"); i >= 0 {
			chunk = chunk[:i]
		}
		if utf8.RuneCountInString(chunk) != got.ReturnedChars {
			t.Fatalf("at offset %d: chunk has %d runes, ReturnedChars says %d",
				offset, utf8.RuneCountInString(chunk), got.ReturnedChars)
		}
		b.WriteString(chunk)
		if !got.Truncated {
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
		ContentTypeJSON:     false,
		"":                  false,
		"html":              false,
	}
	for contentType, want := range cases {
		if got := TruncatableContentType(contentType); got != want {
			t.Errorf("TruncatableContentType(%q): got %v, want %v", contentType, got, want)
		}
	}
}
