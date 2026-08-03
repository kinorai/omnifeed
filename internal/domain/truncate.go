package domain

import (
	"fmt"
	"unicode/utf8"
)

// ContentTypeKey is the Document.Metadata key every engine sets to describe the
// shape of PageContent: "markdown" for prose-ish text, "toon"/"json" for
// structured output. Size control keys off it — see TruncatableContentType.
const ContentTypeKey = "content_type"

// Content types engines report under ContentTypeKey. The Reddit engine also
// reports its format knob verbatim ("json"), which is simply not markdown.
const (
	ContentTypeMarkdown = "markdown"
	ContentTypeTOON     = "toon"
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
// numbers a caller needs to continue where it stopped. Content was cut exactly
// when NextStartChar < TotalChars.
type Truncation struct {
	Text          string
	TotalChars    int // total characters (runes) in the source content
	NextStartChar int // offset to pass as start_char to continue
}

// Truncated reports whether content remains beyond the returned window.
func (t Truncation) Truncated() bool { return t.NextStartChar < t.TotalChars }

// TruncateContent returns the [startChar, startChar+maxChars) character window
// of content, appending a continuation marker when content remains beyond it.
// The marker counts against maxChars — Text never exceeds maxChars runes, so a
// caller-declared ceiling (anthropic/maxResultSizeChars) holds. Offsets and
// lengths are counted in characters (runes), never bytes, so a window never
// splits a multibyte character and the offsets a caller echoes back stay
// meaningful.
//
// maxChars <= 0 means unlimited (startChar is still honored). A startChar at or
// past the end of a non-empty document yields a short explanatory message
// instead of a silently empty result.
func TruncateContent(content string, maxChars, startChar int) Truncation {
	total := utf8.RuneCountInString(content)
	if startChar < 0 {
		startChar = 0
	}

	if startChar > 0 && startChar >= total {
		return Truncation{
			Text:          fmt.Sprintf("[omnifeed: no content at offset %d — total %d characters]", startChar, total),
			TotalChars:    total,
			NextStartChar: total,
		}
	}

	runes := []rune(content)[startChar:]
	if maxChars > 0 && len(runes) > maxChars {
		// Reserve room for the marker inside the window so the total stays
		// within maxChars — the ceiling the caller advertised. Compute the
		// marker against the unshrunk offset first; shrinking can shift the
		// digits by one, which the format absorbs. A budget too small to fit
		// the marker at all overshoots instead: shrinking to nothing would
		// stall the resume loop at the same offset forever.
		marker := truncationMarker(startChar+maxChars, total)
		keep := maxChars - utf8.RuneCountInString(marker)
		if keep < 1 {
			keep = maxChars
		}
		runes = runes[:keep]
		next := startChar + keep
		return Truncation{
			Text:          string(runes) + truncationMarker(next, total),
			TotalChars:    total,
			NextStartChar: next,
		}
	}

	// The window covers everything from startChar to the end: no cut, no marker.
	return Truncation{
		Text:          string(runes),
		TotalChars:    total,
		NextStartChar: total,
	}
}

func truncationMarker(next, total int) string {
	return fmt.Sprintf(
		"\n\n[omnifeed: content truncated at %d of %d characters. Call fetch_url again with start_char=%d to continue.]",
		next, total, next)
}
