package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// fetchFxTwitter reads the post, its parents and the first page of replies
// from FxTwitter's /2/conversation, then the author's self-thread from
// /2/thread when the post belongs to one.
func (e *Engine) fetchFxTwitter(ctx context.Context, id string) (bundle, error) {
	raw, err := e.get(ctx, e.fx, e.fxBase+"/2/conversation/"+id, "fxtwitter")
	if err != nil {
		return bundle{}, err
	}
	conv, err := parseFxConversation(raw)
	if err != nil {
		return bundle{}, err
	}

	root := conv.Status
	b := bundle{Post: fromFx(root), Source: sourceFxTwitter}
	b.Post.TextComplete = true
	var parents []fxStatus
	for i := range conv.Thread {
		if conv.Thread[i].ID != root.ID {
			parents = append(parents, conv.Thread[i])
		}
	}
	replies := make([]fxStatus, 0, len(conv.Replies))
	for i := range conv.Replies {
		if conv.Replies[i].Type != "tombstone" && conv.Replies[i].ID != "" {
			replies = append(replies, conv.Replies[i])
		}
	}

	var chain []fxStatus
	if isSelfThread(root, parents, replies) {
		chain = e.fetchThread(ctx, root)
		if len(chain) < 2 {
			// /2/thread failed or came back empty: the conversation alone
			// still holds the chain's neighbourhood.
			chain = threadFromConversation(root, parents, replies)
		}
	}
	inThread := map[string]bool{}
	for i := range chain {
		inThread[chain[i].ID] = true
	}
	if len(chain) >= 2 {
		for i := range chain {
			if chain[i].ID == root.ID {
				b.Thread = append(b.Thread, b.Post)
				continue
			}
			b.Thread = append(b.Thread, fromFx(&chain[i]))
		}
	}
	for i := range parents {
		if !inThread[parents[i].ID] {
			b.Parents = append(b.Parents, fromFx(&parents[i]))
		}
	}
	for i := range replies {
		if inThread[replies[i].ID] {
			continue
		}
		r := fromFx(&replies[i])
		r.ByAuthor = sameHandle(r.Handle, b.Post.Handle)
		b.Replies = append(b.Replies, r)
	}
	return b, nil
}

// fetchThread reads the author's self-thread around root. A failure is not
// fatal — the caller falls back to the chain visible in the conversation.
func (e *Engine) fetchThread(ctx context.Context, root *fxStatus) []fxStatus {
	raw, err := e.get(ctx, e.fx, e.fxBase+"/2/thread/"+root.ID, "fxtwitter")
	if err != nil {
		e.logger.Warn("fxtwitter thread fetch failed, using the conversation's chain", "id", root.ID, "err", err)
		return nil
	}
	conv, err := parseFxConversation(raw)
	if err != nil {
		e.logger.Warn("fxtwitter thread parse failed, using the conversation's chain", "id", root.ID, "err", err)
		return nil
	}
	var chain []fxStatus
	for i := range conv.Thread {
		if conv.Thread[i].Type != "tombstone" && sameHandle(conv.Thread[i].Author.ScreenName, root.Author.ScreenName) {
			chain = append(chain, conv.Thread[i])
		}
	}
	return chain
}

// parseFxConversation decodes a /2/conversation or /2/thread body. A body
// whose status is null is FxTwitter's "no such post" (it also says so with a
// 404, which get has already turned into an error).
func parseFxConversation(raw []byte) (*fxConversation, error) {
	var conv fxConversation
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("fxtwitter: decode: %w", err)}
	}
	if conv.Status == nil || conv.Status.ID == "" {
		if conv.Code == 404 || conv.Code == 0 {
			return nil, notFoundError("fxtwitter")
		}
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("fxtwitter: no status in response (code %d)", conv.Code)}
	}
	return &conv, nil
}

// isSelfThread reports whether root belongs to its author's self-thread: it
// continues the author's own previous post, or the author continued it.
func isSelfThread(root *fxStatus, parents, replies []fxStatus) bool {
	author := root.Author.ScreenName
	if root.ReplyingTo != nil && len(parents) > 0 {
		last := parents[len(parents)-1]
		if last.ID == root.ReplyingTo.Status && sameHandle(last.Author.ScreenName, author) {
			return true
		}
	}
	for i := range replies {
		if replies[i].ReplyingTo != nil && replies[i].ReplyingTo.Status == root.ID &&
			sameHandle(replies[i].Author.ScreenName, author) {
			return true
		}
	}
	return false
}

// threadFromConversation rebuilds as much of the self-thread as the
// conversation shows: the author's unbroken chain of parents, root, then the
// author's replies each answering the previous link.
func threadFromConversation(root *fxStatus, parents, replies []fxStatus) []fxStatus {
	author := root.Author.ScreenName
	var before []fxStatus
	want := ""
	if root.ReplyingTo != nil {
		want = root.ReplyingTo.Status
	}
	for i := len(parents) - 1; i >= 0 && want != ""; i-- {
		p := parents[i]
		if p.ID != want || !sameHandle(p.Author.ScreenName, author) {
			break
		}
		before = append([]fxStatus{p}, before...)
		want = ""
		if p.ReplyingTo != nil {
			want = p.ReplyingTo.Status
		}
	}
	chain := make([]fxStatus, 0, len(before)+1)
	chain = append(chain, before...)
	chain = append(chain, *root)
	for last := root.ID; ; {
		next := -1
		for i := range replies {
			if replies[i].ReplyingTo != nil && replies[i].ReplyingTo.Status == last &&
				sameHandle(replies[i].Author.ScreenName, author) {
				next = i
				break
			}
		}
		if next < 0 {
			return chain
		}
		chain = append(chain, replies[next])
		last = replies[next].ID
	}
}

// fromFx normalizes an FxTwitter v2 status.
func fromFx(s *fxStatus) post {
	if s.Type == "tombstone" {
		return post{ID: s.ID, Unavailable: nonEmpty(s.Message, "this post is unavailable")}
	}
	p := post{
		ID:          s.ID,
		Handle:      s.Author.ScreenName,
		Name:        s.Author.Name,
		Created:     unixTime(s.CreatedTS, s.CreatedAt),
		Likes:       s.Likes,
		Reposts:     s.Reposts,
		Replies:     s.Replies,
		Quotes:      s.Quotes,
		IsNoteTweet: s.IsNoteTweet,
	}
	if s.Views != nil {
		p.Views = *s.Views
	}
	if s.ReplyingTo != nil {
		p.ReplyToID, p.ReplyTo = s.ReplyingTo.Status, s.ReplyingTo.ScreenName
	}

	// FxTwitter's text already has url facets expanded and media links
	// dropped; expanding again from the facets covers any it left as t.co.
	text := s.Text
	for _, f := range s.RawText.Facets {
		switch {
		case f.Original == "" || !strings.Contains(text, f.Original):
		case f.Type == "url" && f.Replacement != "":
			text = strings.ReplaceAll(text, f.Original, f.Replacement)
		case f.Type == "media":
			text = strings.ReplaceAll(text, f.Original, "")
		}
	}
	p.Text = cleanText(text, p.ReplyTo != "")

	for _, m := range s.Media.All {
		switch m.Type {
		case "photo":
			p.Media = append(p.Media, media{Kind: "image", Alt: m.AltText})
		case "video", "gif":
			p.Media = append(p.Media, media{Kind: m.Type, Alt: m.AltText, Duration: m.Duration})
		}
	}
	if s.Quote != nil {
		if s.Quote.Type == "tombstone" {
			p.QuoteGone = nonEmpty(s.Quote.Message, "the quoted post is unavailable")
		} else {
			q := fromFx(s.Quote)
			q.TextComplete = true
			p.Quote = &q
		}
	}
	if s.CommunityNote != nil {
		p.Note = strings.TrimSpace(s.CommunityNote.Text)
	}
	if s.Poll != nil && len(s.Poll.Choices) > 0 {
		pl := &poll{TotalVotes: s.Poll.TotalVotes, Status: s.Poll.TimeLeftEn}
		for _, c := range s.Poll.Choices {
			pl.Choices = append(pl.Choices, pollChoice{Label: c.Label, Votes: c.Count, Percent: c.Percentage})
		}
		p.Poll = pl
	}
	if s.Card != nil && s.Card.URL != "" {
		p.Card = &card{Title: s.Card.Title, URL: s.Card.URL}
	}
	if s.Article != nil {
		p.Article = &article{Title: s.Article.Title, Markdown: articleMarkdown(s.Article)}
	}
	return p
}
