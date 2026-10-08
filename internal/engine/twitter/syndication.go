package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// syndicationToken is the tweet-result `token` parameter. The endpoint wants
// one but accepts any non-empty value (tested 2026-10-08); an empty token
// returns `{}`.
const syndicationToken = "x"

// fetchSyndication reads the post from the embed syndication endpoint. It cuts
// a long post (note_tweet) at ~275 characters, so for those it asks vxTwitter
// for the full text, and flags the bundle Truncated when that fails too.
func (e *Engine) fetchSyndication(ctx context.Context, id string) (bundle, error) {
	q := url.Values{}
	q.Set("id", id)
	q.Set("token", syndicationToken)
	raw, err := e.get(ctx, e.synd, e.syndBase+"/tweet-result?"+q.Encode(), "syndication")
	if err != nil {
		return bundle{}, err
	}
	st, err := parseSyndication(raw)
	if err != nil {
		return bundle{}, err
	}

	b := bundle{Post: fromSynd(st), Source: sourceSyndication}
	if st.Parent != nil && st.Parent.IDStr != "" {
		b.Parents = []post{fromSynd(st.Parent)}
	}
	if !b.Post.TextComplete {
		vx, verr := e.fetchVx(ctx, id)
		switch {
		case verr == nil && len([]rune(vx.Text)) > len([]rune(b.Post.Text)):
			b.Post.Text, b.Post.TextComplete = vx.Text, true
			b.Source = sourceSyndicationVx
		case verr != nil:
			e.logger.Warn("vxtwitter full-text fetch failed; keeping the cut syndication text", "id", id, "err", verr)
		}
	}
	b.Truncated = !b.Post.TextComplete
	return b, nil
}

// parseSyndication decodes a tweet-result body. A TweetTombstone is a post X
// withholds (deleted, protected, or its account suspended).
func parseSyndication(raw []byte) (*syndTweet, error) {
	var st syndTweet
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("syndication: decode: %w", err)}
	}
	if st.Typename == "TweetTombstone" || st.Tombstone != nil {
		return nil, notFoundError("syndication")
	}
	if st.IDStr == "" {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("syndication: empty tweet in response")}
	}
	return &st, nil
}

// fromSynd normalizes a syndication tweet.
func fromSynd(st *syndTweet) post {
	p := post{
		ID:           st.IDStr,
		Handle:       st.User.ScreenName,
		Name:         st.User.Name,
		Likes:        st.FavoriteCount,
		Replies:      st.ConversationCnt,
		ReplyToID:    st.InReplyToStatus,
		ReplyTo:      st.InReplyToScreen,
		TextComplete: st.NoteTweet == nil,
	}
	if t, err := time.Parse(time.RFC3339, st.CreatedAt); err == nil {
		p.Created = t.UTC().Format(time.RFC3339)
	}

	text := st.Text
	for _, u := range st.Entities.URLs {
		if u.URL != "" && u.ExpandedURL != "" {
			text = strings.ReplaceAll(text, u.URL, u.ExpandedURL)
		}
	}
	for _, m := range st.Entities.Media {
		if m.URL != "" {
			text = strings.ReplaceAll(text, m.URL, "")
		}
	}
	for _, m := range st.MediaDetails {
		if m.URL != "" {
			text = strings.ReplaceAll(text, m.URL, "")
		}
	}
	p.Text = cleanText(text, p.ReplyTo != "")

	for _, m := range st.MediaDetails {
		switch m.Type {
		case "photo":
			p.Media = append(p.Media, media{Kind: "image", Alt: m.ExtAltText})
		case "video", "animated_gif":
			md := media{Kind: "video", Alt: m.ExtAltText}
			if m.Type == "animated_gif" {
				md.Kind = "gif"
			}
			if m.VideoInfoMS != nil {
				md.Duration = float64(m.VideoInfoMS.DurationMillis) / 1000
			}
			p.Media = append(p.Media, md)
		}
	}
	if st.QuotedTweet != nil && st.QuotedTweet.IDStr != "" {
		q := fromSynd(st.QuotedTweet)
		p.Quote = &q
	}
	if st.BirdwatchPivot != nil {
		p.Note = strings.TrimSpace(st.BirdwatchPivot.Subtitle.Text)
	}
	if st.Article != nil && st.Article.Title != "" {
		// Syndication carries only the article's preview, never its body.
		p.Article = &article{Title: st.Article.Title, Markdown: strings.TrimSpace(st.Article.PreviewText) + " …"}
		p.TextComplete = false
	}
	if st.Card != nil {
		p.Poll, p.Card = syndCardParts(st.Card)
	}
	return p
}

// syndCardParts reads a syndication card's binding values: a poll card
// (choiceN_label / choiceN_count) or a link preview (title + card_url).
func syndCardParts(c *syndCard) (*poll, *card) {
	val := func(k string) string { return c.BindingValues[k].StringValue }
	if val("choice1_label") != "" {
		pl := &poll{}
		for i := 1; i <= 4; i++ {
			label := val("choice" + strconv.Itoa(i) + "_label")
			if label == "" {
				break
			}
			n, _ := strconv.Atoi(val("choice" + strconv.Itoa(i) + "_count"))
			pl.Choices = append(pl.Choices, pollChoice{Label: label, Votes: n})
			pl.TotalVotes += n
		}
		for i := range pl.Choices {
			if pl.TotalVotes > 0 {
				pl.Choices[i].Percent = float64(pl.Choices[i].Votes) * 100 / float64(pl.TotalVotes)
			}
		}
		if c.BindingValues["counts_are_final"].BooleanValue {
			pl.Status = "final results"
		}
		return pl, nil
	}
	u := nonEmpty(val("card_url"), c.URL)
	if title := val("title"); title != "" && u != "" {
		return nil, &card{Title: title, URL: u}
	}
	return nil, nil
}
