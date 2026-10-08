package twitter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/toon-format/toon-go"
)

// explicitFormat is the structured format ("toon" | "json") the caller passed,
// or "" when it named none. RedditFormat alone carries the deployment default
// (Reddit's TOON); X posts default to markdown, so only an explicit format
// (FormatExplicit) switches them.
func explicitFormat(opts domain.EngineOptions) string {
	if !opts.FormatExplicit {
		return ""
	}
	return opts.RedditFormat
}

// document renders a bundle as markdown (the default) or, when the caller
// asked for one, TOON or JSON, and attaches the metadata.
func (e *Engine) document(b bundle, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	replies := b.Replies
	if len(replies) > e.maxReplies {
		replies = replies[:e.maxReplies]
	}

	format := explicitFormat(opts)
	var content, contentType string
	switch format {
	case "toon", "json":
		out := structured(b, replies)
		var raw []byte
		var err error
		if format == "json" {
			raw, err = json.Marshal(out)
		} else {
			raw, err = toon.Marshal(out)
		}
		if err != nil {
			return domain.Document{}, fmt.Errorf("encode: %w", err)
		}
		content, contentType = string(raw), format
	default:
		content, contentType = renderMarkdown(b, replies), domain.ContentTypeMarkdown
	}

	meta := map[string]string{
		"source":              rawURL,
		"status_code":         "200",
		domain.ContentTypeKey: contentType,
		"id":                  b.Post.ID,
		"author":              b.Post.Handle,
		"upstream":            b.Source,
		"replies":             strconv.Itoa(len(replies)),
		"reply_total":         strconv.Itoa(b.Post.Replies),
		"text_truncated":      strconv.FormatBool(b.Truncated),
	}
	if len(b.Thread) > 0 {
		meta["thread_posts"] = strconv.Itoa(len(b.Thread))
	}
	e.logger.Info("twitter crawl complete", "source", rawURL, "upstream", b.Source, "bytes", len(content))
	return domain.Document{PageContent: content, Metadata: meta}, nil
}

// renderMarkdown lays the post out for an LLM reader: header line, parents,
// text, attachments, article, self-thread, replies.
func renderMarkdown(b bundle, replies []post) string {
	p := b.Post
	var w strings.Builder

	w.WriteString(header(p, true) + "\n")
	if b.Truncated {
		w.WriteString("\n**Text truncated:** only the first ~275 characters of this post were available (" + b.Source + " fallback).\n")
	}

	if len(b.Parents) > 0 {
		w.WriteString("\n**In reply to:**\n")
		for _, par := range b.Parents {
			w.WriteString("\n" + quoteBlock(par) + "\n")
		}
	}

	if body := postBody(p); body != "" {
		w.WriteString("\n" + body + "\n")
	}

	if p.Article != nil {
		w.WriteString("\n## Article: " + p.Article.Title + "\n\n" + p.Article.Markdown + "\n")
	}

	if len(b.Thread) > 0 {
		fmt.Fprintf(&w, "\n## Thread by @%s (%d posts)\n", p.Handle, len(b.Thread))
		for i, tp := range b.Thread {
			n := strconv.Itoa(i+1) + ". "
			if tp.ID == p.ID {
				w.WriteString("\n" + n + "(the linked post, above)\n")
				continue
			}
			if tp.Unavailable != "" {
				w.WriteString("\n" + n + "[" + tp.Unavailable + "]\n")
				continue
			}
			w.WriteString("\n" + n + indent(threadItem(tp), strings.Repeat(" ", len(n))) + "\n")
		}
	}

	if len(replies) > 0 {
		fmt.Fprintf(&w, "\n## Replies (%d of %d)\n\n", len(replies), max(p.Replies, len(replies)))
		for _, r := range replies {
			w.WriteString("- " + indent(replyLine(r), "  ") + "\n")
		}
	}
	return strings.TrimSpace(w.String()) + "\n"
}

// header is the one-line identity of a post: who, when, where, how much
// engagement.
func header(p post, counts bool) string {
	parts := []string{author(p)}
	if p.Created != "" {
		parts = append(parts, p.Created)
	}
	parts = append(parts, postURL(p.Handle, p.ID))
	if counts {
		parts = append(parts, plural(p.Likes, "like"))
		if p.Reposts > 0 {
			parts = append(parts, plural(p.Reposts, "repost"))
		}
		if p.Replies > 0 {
			parts = append(parts, plural(p.Replies, "reply"))
		}
		if p.Views > 0 {
			parts = append(parts, plural(p.Views, "view"))
		}
	}
	return strings.Join(parts, " · ")
}

func author(p post) string {
	if p.Name != "" && p.Name != p.Handle {
		return "@" + p.Handle + " (" + p.Name + ")"
	}
	return "@" + p.Handle
}

// postBody is the text and every attachment of the linked post.
func postBody(p post) string {
	var parts []string
	if p.Text != "" {
		parts = append(parts, p.Text)
	}
	if m := mediaLine(p.Media); m != "" {
		parts = append(parts, m)
	}
	if p.Quote != nil {
		parts = append(parts, "Quoting:\n\n"+quoteBlock(*p.Quote))
	} else if p.QuoteGone != "" {
		parts = append(parts, "> [quoted post unavailable: "+p.QuoteGone+"]")
	}
	if p.Poll != nil {
		parts = append(parts, pollBlock(p.Poll))
	}
	if p.Note != "" {
		parts = append(parts, "**Community note:** "+p.Note)
	}
	if p.Card != nil {
		parts = append(parts, "**Link:** "+cardLine(p.Card))
	}
	return strings.Join(parts, "\n\n")
}

// quoteBlock renders a quoted or parent post as a markdown blockquote.
func quoteBlock(p post) string {
	if p.Unavailable != "" {
		return "> [" + p.Unavailable + "]"
	}
	var lines []string
	lines = append(lines, header(p, false))
	if p.Text != "" {
		lines = append(lines, "", p.Text)
	}
	if m := mediaLine(p.Media); m != "" {
		lines = append(lines, "", m)
	}
	if p.Poll != nil {
		lines = append(lines, "", pollBlock(p.Poll))
	}
	if p.Card != nil {
		lines = append(lines, "", "Link: "+cardLine(p.Card))
	}
	if p.Quote != nil {
		lines = append(lines, "", "(quoting "+postURL(p.Quote.Handle, p.Quote.ID)+")")
	}
	return blockquote(strings.Join(lines, "\n"))
}

// blockquote prefixes every line with "> " (a bare ">" on blank lines, so
// no line carries trailing whitespace).
func blockquote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

// threadItem is one self-thread post: text plus attachments, no header (the
// author is the thread's).
func threadItem(p post) string {
	parts := []string{}
	if p.Text != "" {
		parts = append(parts, p.Text)
	}
	if m := mediaLine(p.Media); m != "" {
		parts = append(parts, m)
	}
	if p.Quote != nil {
		parts = append(parts, quoteBlock(*p.Quote))
	}
	if p.Card != nil {
		parts = append(parts, "Link: "+cardLine(p.Card))
	}
	if len(parts) == 0 {
		return "(no text)"
	}
	return strings.Join(parts, "\n\n")
}

// replyLine is `@handle (N likes): text [media]`, with the linked post's
// author marked.
func replyLine(r post) string {
	who := "@" + r.Handle
	if r.ByAuthor {
		who += " [author]"
	}
	line := who + " (" + plural(r.Likes, "like") + "): " + compactLines(r.Text)
	if m := mediaLine(r.Media); m != "" {
		line += " " + m
	}
	if r.Quote != nil {
		line += " (quoting " + author(*r.Quote) + ": " + oneLine(r.Quote.Text, 200) + ")"
	}
	return line
}

func mediaLine(ms []media) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, mediaMarker(m))
	}
	return strings.Join(parts, " ")
}

// mediaMarker is `[image: alt]`, `[video, 1:23]`, `[gif]`.
func mediaMarker(m media) string {
	s := m.Kind
	if m.Kind != "image" && m.Duration > 0 {
		s += ", " + duration(m.Duration)
	}
	if alt := oneLine(m.Alt, 0); alt != "" {
		s += ": " + alt
	}
	return "[" + s + "]"
}

// duration renders seconds as m:ss, or h:mm:ss past an hour.
func duration(sec float64) string {
	t := int(sec + 0.5)
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

func pollBlock(pl *poll) string {
	head := "**Poll**"
	var info []string
	if pl.TotalVotes > 0 {
		info = append(info, plural(pl.TotalVotes, "vote"))
	}
	if pl.Status != "" {
		info = append(info, pl.Status)
	}
	if len(info) > 0 {
		head += " (" + strings.Join(info, ", ") + ")"
	}
	lines := []string{head + ":"}
	for _, c := range pollLines(pl) {
		lines = append(lines, "- "+c)
	}
	return strings.Join(lines, "\n")
}

func pollLines(pl *poll) []string {
	out := make([]string, 0, len(pl.Choices))
	for _, c := range pl.Choices {
		out = append(out, fmt.Sprintf("%s: %.1f%% (%s)", c.Label, c.Percent, plural(c.Votes, "vote")))
	}
	return out
}

func cardLine(c *card) string {
	if c.Title == "" {
		return c.URL
	}
	return "[" + c.Title + "](" + c.URL + ")"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") {
		return strconv.Itoa(n) + " " + strings.TrimSuffix(word, "y") + "ies"
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// compactLines drops blank lines, so a reply's paragraphs stay one tight
// list item.
func compactLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// indent prefixes every line after the first, so a multi-line text stays
// inside its list item.
// Blank lines stay empty rather than carry trailing padding.
func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = pad + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// oneLine collapses whitespace and cuts s at limit runes (0 = no cut).
func oneLine(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); limit > 0 && len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}

// structured converts a bundle to the TOON/JSON output shape.
func structured(b bundle, replies []post) Output {
	out := Output{Post: outPost(b.Post), Source: b.Source, Truncated: b.Truncated}
	for _, p := range b.Parents {
		out.Parents = append(out.Parents, outPost(p))
	}
	for _, p := range b.Thread {
		out.Thread = append(out.Thread, outPost(p))
	}
	for _, p := range replies {
		out.Replies = append(out.Replies, outPost(p))
	}
	return out
}

func outPost(p post) OutPost {
	if p.Unavailable != "" {
		return OutPost{ID: p.ID, URL: canonicalURL(p.ID), Text: "[" + p.Unavailable + "]"}
	}
	o := OutPost{
		ID: p.ID, URL: postURL(p.Handle, p.ID), Author: p.Handle, Name: p.Name,
		CreatedAt: p.Created, Text: p.Text, Likes: p.Likes, Reposts: p.Reposts,
		Replies: p.Replies, Quotes: p.Quotes, Views: p.Views, ReplyTo: p.ReplyToID,
		ByAuthor: p.ByAuthor, Note: p.Note,
	}
	for _, m := range p.Media {
		o.Media = append(o.Media, mediaMarker(m))
	}
	if p.Quote != nil {
		q := outPost(*p.Quote)
		o.Quote = &q
	} else if p.QuoteGone != "" {
		o.Quote = &OutPost{Text: "[quoted post unavailable: " + p.QuoteGone + "]"}
	}
	if p.Poll != nil {
		o.Poll = pollLines(p.Poll)
	}
	if p.Card != nil {
		o.Card = cardLine(p.Card)
	}
	if p.Article != nil {
		o.Article = "# " + p.Article.Title + "\n\n" + p.Article.Markdown
	}
	return o
}
