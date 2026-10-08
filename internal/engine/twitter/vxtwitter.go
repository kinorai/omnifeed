package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// fetchVx reads a post from the vxTwitter API. It has the full text of long
// posts and the quoted post, but no thread and no replies.
func (e *Engine) fetchVx(ctx context.Context, id string) (post, error) {
	raw, err := e.get(ctx, e.vx, e.vxBase+"/Twitter/status/"+id, "vxtwitter")
	if err != nil {
		return post{}, err
	}
	var vt vxTweet
	if err := json.Unmarshal(raw, &vt); err != nil {
		return post{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("vxtwitter: decode: %w", err)}
	}
	if vt.TweetID == "" {
		return post{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("vxtwitter: empty tweet in response")}
	}
	return fromVx(&vt), nil
}

// fetchVxBundle is the last API source: vxTwitter alone, when FxTwitter and
// syndication both failed.
func (e *Engine) fetchVxBundle(ctx context.Context, id string) (bundle, error) {
	p, err := e.fetchVx(ctx, id)
	if err != nil {
		return bundle{}, err
	}
	return bundle{Post: p, Source: sourceVxTwitter}, nil
}

// fromVx normalizes a vxTwitter tweet.
func fromVx(vt *vxTweet) post {
	p := post{
		ID:           vt.TweetID,
		Handle:       vt.UserScreen,
		Name:         vt.UserName,
		Created:      unixTime(vt.DateEpoch, vt.Date),
		Likes:        vt.Likes,
		Reposts:      vt.Retweets,
		Replies:      vt.Replies,
		ReplyToID:    vt.ReplyingToID,
		ReplyTo:      vt.ReplyingTo,
		TextComplete: true,
	}
	p.Text = cleanText(vt.Text, p.ReplyTo != "")
	var note string
	if json.Unmarshal(vt.CommunityNote, &note) == nil {
		p.Note = strings.TrimSpace(note)
	}
	for _, m := range vt.MediaExtended {
		switch m.Type {
		case "image":
			p.Media = append(p.Media, media{Kind: "image", Alt: m.AltText})
		case "video", "gif":
			p.Media = append(p.Media, media{Kind: m.Type, Alt: m.AltText, Duration: m.DurationMS / 1000})
		}
	}
	if vt.QRT != nil && vt.QRT.TweetID != "" {
		q := fromVx(vt.QRT)
		p.Quote = &q
	}
	var pd vxPollData
	if json.Unmarshal(vt.PollData, &pd) == nil && len(pd.Options) > 0 {
		pl := &poll{}
		for _, o := range pd.Options {
			pl.Choices = append(pl.Choices, pollChoice{Label: o.Name, Votes: o.Votes, Percent: o.Percent})
			pl.TotalVotes += o.Votes
		}
		p.Poll = pl
	}
	return p
}
