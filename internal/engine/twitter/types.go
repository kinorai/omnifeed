// Package twitter implements the Twitter/X engine. It renders an x.com (or
// twitter.com, fxtwitter.com, fixupx.com, vxtwitter.com, fixvx.com) post URL
// as the post itself — full text, quote, media with alt text, community note,
// poll, link card, X Article body — plus its parents, the author's
// self-thread and the top replies. Markdown by default; TOON or JSON when the
// caller passes `format`.
//
// x.com is bot-walled for headless browsers and twitter.com links fail in
// crawl4ai outright, so the engine reads public JSON mirrors DIRECTLY over
// HTTP, in order:
//
//  1. FxTwitter API v2 (api.fxtwitter.com, or a self-hosted FxEmbed via
//     OMNIFEED_TWITTER_FXTWITTER_URL): /2/conversation for the post, its
//     parents and the first page of replies; /2/thread when the post belongs
//     to the author's self-thread.
//  2. The embed syndication endpoint (cdn.syndication.twimg.com), which cuts
//     long posts at ~275 characters — so for a note_tweet the vxTwitter API
//     (api.vxtwitter.com) is asked for the full text, and the document is
//     flagged text_truncated when neither could supply it.
//  3. The generic crawl4ai engine on the canonical x.com URL, which X serves
//     server-rendered to logged-out readers.
//
// When every source fails the engine returns a classified "tweet unavailable"
// error — never a near-empty document — marked domain.NoFallback so the
// registry does not render the URL a second time.
package twitter

import "encoding/json"

// --- Upstream: FxTwitter API v2 ---------------------------------------------

// fxConversation is the /2/conversation and /2/thread response. thread holds
// the post's ancestors and the post itself (conversation), or the author's
// whole self-thread (thread).
type fxConversation struct {
	Code    int        `json:"code"`
	Status  *fxStatus  `json:"status"`
	Thread  []fxStatus `json:"thread"`
	Replies []fxStatus `json:"replies"`
}

// fxStatus is one post in an FxTwitter v2 response. A tombstone (deleted or
// protected quote/thread member) has Type "tombstone" and a Message.
type fxStatus struct {
	Type          string         `json:"type"`
	Message       string         `json:"message"`
	ID            string         `json:"id"`
	URL           string         `json:"url"`
	Text          string         `json:"text"`
	RawText       fxRawText      `json:"raw_text"`
	Author        fxUser         `json:"author"`
	CreatedAt     string         `json:"created_at"`
	CreatedTS     int64          `json:"created_timestamp"`
	Likes         int            `json:"likes"`
	Reposts       int            `json:"reposts"`
	Replies       int            `json:"replies"`
	Quotes        int            `json:"quotes"`
	Views         *int           `json:"views"`
	IsNoteTweet   bool           `json:"is_note_tweet"`
	ReplyingTo    *fxReplyingTo  `json:"replying_to"`
	Quote         *fxStatus      `json:"quote"`
	Media         fxMedia        `json:"media"`
	Poll          *fxPoll        `json:"poll"`
	CommunityNote *fxNote        `json:"community_note"`
	Card          *fxCard        `json:"card"`
	Article       *fxArticle     `json:"article"`
	Translation   *fxTranslation `json:"translation"`
}

type fxRawText struct {
	Text   string    `json:"text"`
	Facets []fxFacet `json:"facets"`
}

type fxFacet struct {
	Type        string `json:"type"`
	Original    string `json:"original"`
	Replacement string `json:"replacement"`
}

type fxUser struct {
	ScreenName string `json:"screen_name"`
	Name       string `json:"name"`
}

type fxReplyingTo struct {
	ScreenName string `json:"screen_name"`
	Status     string `json:"status"`
}

type fxMedia struct {
	All []fxMediaItem `json:"all"`
}

type fxMediaItem struct {
	Type     string  `json:"type"` // photo | video | gif
	URL      string  `json:"url"`
	AltText  string  `json:"altText"`
	Duration float64 `json:"duration"` // seconds, videos only
}

type fxPoll struct {
	Choices []struct {
		Label      string  `json:"label"`
		Count      int     `json:"count"`
		Percentage float64 `json:"percentage"`
	} `json:"choices"`
	TotalVotes int    `json:"total_votes"`
	EndsAt     string `json:"ends_at"`
	TimeLeftEn string `json:"time_left_en"`
}

type fxNote struct {
	Text string `json:"text"`
}

type fxCard struct {
	URL         string `json:"url"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type fxTranslation struct {
	Text string `json:"text"`
}

// fxArticle is an X Article: a Draft.js document under content.
type fxArticle struct {
	Title       string `json:"title"`
	PreviewText string `json:"preview_text"`
	Content     struct {
		Blocks    []articleBlock  `json:"blocks"`
		EntityMap []articleEntity `json:"entityMap"`
	} `json:"content"`
	CoverMedia    articleMedia   `json:"cover_media"`
	MediaEntities []articleMedia `json:"media_entities"`
}

type articleBlock struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	EntityRanges []struct {
		Key    int `json:"key"`
		Offset int `json:"offset"`
		Length int `json:"length"`
	} `json:"entityRanges"`
}

type articleEntity struct {
	Key   string `json:"key"`
	Value struct {
		Type string `json:"type"` // LINK | MEDIA | TWEET | MARKDOWN
		Data struct {
			URL        string `json:"url"`
			TweetID    string `json:"tweetId"`
			Markdown   string `json:"markdown"`
			MediaItems []struct {
				MediaID string `json:"mediaId"`
			} `json:"mediaItems"`
		} `json:"data"`
	} `json:"value"`
}

type articleMedia struct {
	MediaID   string `json:"media_id"`
	MediaInfo struct {
		Typename string `json:"__typename"`
		ImageURL string `json:"original_img_url"`
		AltText  string `json:"ext_alt_text"`
	} `json:"media_info"`
}

// --- Upstream: syndication (cdn.syndication.twimg.com/tweet-result) ---------

type syndTweet struct {
	Typename         string          `json:"__typename"`
	IDStr            string          `json:"id_str"`
	Text             string          `json:"text"`
	CreatedAt        string          `json:"created_at"`
	FavoriteCount    int             `json:"favorite_count"`
	ConversationCnt  int             `json:"conversation_count"`
	User             syndUser        `json:"user"`
	Entities         syndEntities    `json:"entities"`
	MediaDetails     []syndMedia     `json:"mediaDetails"`
	QuotedTweet      *syndTweet      `json:"quoted_tweet"`
	Parent           *syndTweet      `json:"parent"`
	NoteTweet        *struct{}       `json:"note_tweet"`
	InReplyToScreen  string          `json:"in_reply_to_screen_name"`
	InReplyToStatus  string          `json:"in_reply_to_status_id_str"`
	Card             *syndCard       `json:"card"`
	Tombstone        *syndTombstone  `json:"tombstone"`
	DisplayTextRange []int           `json:"display_text_range"`
	BirdwatchPivot   *syndBirdwatch  `json:"birdwatch_pivot"`
	Article          *syndArticleRef `json:"article"`
}

type syndUser struct {
	ScreenName string `json:"screen_name"`
	Name       string `json:"name"`
}

type syndEntities struct {
	URLs []struct {
		URL         string `json:"url"`
		ExpandedURL string `json:"expanded_url"`
	} `json:"urls"`
	Media []struct {
		URL string `json:"url"`
	} `json:"media"`
}

type syndMedia struct {
	Type        string `json:"type"` // photo | video | animated_gif
	URL         string `json:"url"`  // the t.co link in the text
	MediaURL    string `json:"media_url_https"`
	ExtAltText  string `json:"ext_alt_text"`
	VideoInfoMS *struct {
		DurationMillis int `json:"duration_millis"`
	} `json:"video_info"`
}

type syndCard struct {
	URL           string `json:"url"`
	BindingValues map[string]struct {
		StringValue  string `json:"string_value"`
		BooleanValue bool   `json:"boolean_value"`
	} `json:"binding_values"`
}

type syndTombstone struct {
	Text struct {
		Text string `json:"text"`
	} `json:"text"`
}

type syndBirdwatch struct {
	Subtitle struct {
		Text string `json:"text"`
	} `json:"subtitle"`
}

type syndArticleRef struct {
	Title       string `json:"title"`
	PreviewText string `json:"preview_text"`
}

// --- Upstream: vxTwitter (api.vxtwitter.com/Twitter/status/{id}) ------------

type vxTweet struct {
	TweetID       string    `json:"tweetID"`
	TweetURL      string    `json:"tweetURL"`
	Text          string    `json:"text"`
	Date          string    `json:"date"`
	DateEpoch     int64     `json:"date_epoch"`
	Likes         int       `json:"likes"`
	Retweets      int       `json:"retweets"`
	Replies       int       `json:"replies"`
	UserName      string    `json:"user_name"`
	UserScreen    string    `json:"user_screen_name"`
	ReplyingTo    string    `json:"replyingTo"`
	ReplyingToID  string    `json:"replyingToID"`
	MediaExtended []vxMedia `json:"media_extended"`
	QRT           *vxTweet  `json:"qrt"`
	// CommunityNote and PollData are decoded leniently (see fromVx): their
	// shapes are undocumented, and a surprise there must not cost the text.
	CommunityNote json.RawMessage `json:"communityNote"`
	PollData      json.RawMessage `json:"pollData"`
}

type vxMedia struct {
	Type       string  `json:"type"` // image | video | gif
	URL        string  `json:"url"`
	AltText    string  `json:"altText"`
	DurationMS float64 `json:"duration_millis"`
}

type vxPollData struct {
	Options []struct {
		Name    string  `json:"name"`
		Votes   int     `json:"votes"`
		Percent float64 `json:"percent"`
	} `json:"options"`
}

// --- Normalized model ---------------------------------------------------------

// post is one tweet, normalized across the three upstream shapes.
type post struct {
	ID           string
	Handle       string
	Name         string
	Created      string // RFC 3339, UTC
	Text         string
	Likes        int
	Reposts      int
	Replies      int
	Quotes       int
	Views        int // 0 = unknown
	ReplyToID    string
	ReplyTo      string // handle of the replied-to account
	Media        []media
	Quote        *post
	QuoteGone    string // tombstone message when the quoted post is unavailable
	Note         string // community note
	Poll         *poll
	Card         *card
	Article      *article
	IsNoteTweet  bool
	Unavailable  string // tombstone message for a thread member that is gone
	ByAuthor     bool   // reply written by the linked post's author
	TextComplete bool   // false when only a cut text was available
}

type media struct {
	Kind     string // image | video | gif
	Alt      string
	Duration float64 // seconds
}

type poll struct {
	Choices    []pollChoice
	TotalVotes int
	Status     string // "final" or the time left
}

type pollChoice struct {
	Label   string
	Votes   int
	Percent float64
}

type card struct {
	Title string
	URL   string
}

type article struct {
	Title    string
	Markdown string
}

// bundle is everything fetched for one linked post. It is what the TTL cache
// keeps, so the output format can vary per request without refetching.
type bundle struct {
	Post      post
	Parents   []post // ancestors, root first
	Thread    []post // the author's self-thread including Post, in order; nil when not a thread
	Replies   []post // first page of replies, upstream order, thread members removed
	Source    string // fxtwitter | syndication | syndication+vxtwitter | vxtwitter
	Truncated bool   // the post text is a cut prefix
}

// --- Output (TOON / JSON) ---------------------------------------------------

// Output is the structured rendering of a bundle.
type Output struct {
	Post      OutPost   `json:"post" toon:"post"`
	Parents   []OutPost `json:"parents,omitempty" toon:"parents,omitempty"`
	Thread    []OutPost `json:"thread,omitempty" toon:"thread,omitempty"`
	Replies   []OutPost `json:"replies,omitempty" toon:"replies,omitempty"`
	Source    string    `json:"source" toon:"source"`
	Truncated bool      `json:"truncated" toon:"truncated"`
}

// OutPost is one post in the structured output. Media, Poll and Card are
// pre-rendered one-liners — the same strings the markdown shows — so an LLM
// reads one vocabulary whichever format it asked for.
type OutPost struct {
	ID        string   `json:"id" toon:"id"`
	URL       string   `json:"url" toon:"url"`
	Author    string   `json:"author" toon:"author"`
	Name      string   `json:"name,omitempty" toon:"name,omitempty"`
	CreatedAt string   `json:"created_at,omitempty" toon:"created_at,omitempty"`
	Text      string   `json:"text" toon:"text"`
	Likes     int      `json:"likes" toon:"likes"`
	Reposts   int      `json:"reposts,omitempty" toon:"reposts,omitempty"`
	Replies   int      `json:"replies,omitempty" toon:"replies,omitempty"`
	Quotes    int      `json:"quotes,omitempty" toon:"quotes,omitempty"`
	Views     int      `json:"views,omitempty" toon:"views,omitempty"`
	ReplyTo   string   `json:"reply_to,omitempty" toon:"reply_to,omitempty"`
	ByAuthor  bool     `json:"by_author,omitempty" toon:"by_author,omitempty"`
	Media     []string `json:"media,omitempty" toon:"media,omitempty"`
	Quote     *OutPost `json:"quote,omitempty" toon:"quote,omitempty"`
	Note      string   `json:"community_note,omitempty" toon:"community_note,omitempty"`
	Poll      []string `json:"poll,omitempty" toon:"poll,omitempty"`
	Card      string   `json:"card,omitempty" toon:"card,omitempty"`
	Article   string   `json:"article,omitempty" toon:"article,omitempty"`
}
