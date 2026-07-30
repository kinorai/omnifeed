package domain

import (
	"fmt"
	"unicode/utf8"
)

// ContentTypeKey is the Document.Metadata key every engine sets to describe the
// shape of PageContent: "markdown" for prose-ish text, "toon"/"json" for
// structured output. Size control keys off it — see TruncatableContentType.
const ContentTypeKey = "content_type"

// Content types engines report under ContentTypeKey.
const (
	ContentTypeMarkdown = "markdown"
	ContentTypeTOON     = "toon"
	ContentTypeJSON     = "json"
)

// TruncatableContentType reports whether generic char truncation is safe for a
// content type. Only markdown is: TOON and JSON carry length markers / closing
// delimiters, so cutting them mid-document produces a payload that lies about
// its own contents. Structured engines bound their output with their own
// element caps instead (see the Reddit knobs).
func TruncatableContentType(contentType string) bool {
	return contentType == ContentTypeMarkdown
}

// Truncation is the outcome of TruncateContent: the text to hand back plus the
// numbers a caller needs to continue where it stopped.
type Truncation struct {
	Text          string
	Truncated     bool // content remains beyond the returned slice
	TotalChars    int  // total characters (runes) in the source content
	ReturnedChars int  // characters actually returned, marker excluded
	NextStartChar int  // offset to pass as start_char to continue
}

// TruncateContent returns the [startChar, startChar+maxChars) character window
// of content, appending a continuation marker when content remains beyond it.
// Offsets and lengths are counted in characters (runes), never bytes, so a
// window never splits a multibyte character and the offsets a caller echoes
// back stay meaningful.
//
// maxChars <= 0 means unlimited (startChar is still honored). A startChar at or
// past the end of a non-empty document yields a short explanatory message and
// ReturnedChars 0 instead of a silently empty result.
func TruncateContent(content string, maxChars, startChar int) Truncation {
	total := utf8.RuneCountInString(content)
	if startChar < 0 {
		startChar = 0
	}

	if startChar > 0 && startChar >= total {
		return Truncation{
			Text:          fmt.Sprintf("[omnifeed: no content at offset %d — total %d characters]", startChar, total),
			TotalChars:    total,
			ReturnedChars: 0,
			NextStartChar: total,
		}
	}

	runes := []rune(content)[startChar:]
	if maxChars > 0 && len(runes) > maxChars {
		runes = runes[:maxChars]
	}

	next := startChar + len(runes)
	out := Truncation{
		Text:          string(runes),
		Truncated:     next < total,
		TotalChars:    total,
		ReturnedChars: len(runes),
		NextStartChar: next,
	}
	if out.Truncated {
		out.Text += fmt.Sprintf(
			"\n\n[omnifeed: content truncated at %d of %d characters. Call fetch_url again with start_char=%d to continue.]",
			next, total, next)
	}
	return out
}
