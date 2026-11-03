// Package stackexchange implements the Stack Exchange engine. It reads the
// public Stack Exchange API (api.stackexchange.com, v2.3) and renders a
// question URL on Stack Overflow, Super User, Server Fault, Ask Ubuntu,
// MathOverflow or any {name}.stackexchange.com site as the question header +
// body and its answers by votes (TOON), headed by a "# title" line.
//
// Like the Hacker News, GitHub and Discourse engines (and unlike Reddit and the
// generic engine), this engine fetches its upstream DIRECTLY over HTTP rather
// than through crawl4ai: the question pages sit behind Cloudflare, which
// answers crawl4ai with a 403 ("Blocked by anti-bot protection"), while the API
// serves the same content keyless. It does require omnifeed to have outbound
// access to api.stackexchange.com.
//
// Keyless callers share a quota of 300 requests a day per IP; a key
// (OMNIFEED_STACKEXCHANGE_KEY) raises it to 10,000. A question costs 2
// requests (question + answers), a short /a/ link 3.
package stackexchange

// Question is the header of a question, stripped to LLM-relevant fields.
// AcceptedAnswer is the id of the accepted answer (0 = none), the same id the
// accepted entry in Answers carries.
type Question struct {
	Title          string   `json:"title" toon:"title"`
	Site           string   `json:"site" toon:"site"`
	Score          int      `json:"score" toon:"score"`
	Tags           []string `json:"tags,omitempty" toon:"tags,omitempty"`
	Author         string   `json:"author" toon:"author"`
	Asked          string   `json:"asked" toon:"asked"`
	Answers        int      `json:"answers" toon:"answers"`
	AcceptedAnswer int64    `json:"accepted_answer,omitempty" toon:"accepted_answer,omitempty"`
	Closed         string   `json:"closed,omitempty" toon:"closed,omitempty"`
	Body           string   `json:"body" toon:"body"`
}

// Answer is one answer, in vote order. Accepted marks the answer the asker
// accepted.
type Answer struct {
	ID       int64  `json:"id" toon:"id"`
	Score    int    `json:"score" toon:"score"`
	Accepted bool   `json:"accepted" toon:"accepted"`
	Author   string `json:"author" toon:"author"`
	Created  string `json:"created" toon:"created"`
	Body     string `json:"body" toon:"body"`
}

// Thread groups a question with its answers. Note, when set, tells the reader
// (the LLM) that the answer list was truncated — metadata keys land in MCP
// _meta, which models never see, so the signal has to live in the content
// itself.
type Thread struct {
	Question Question `json:"question" toon:"question"`
	Note     string   `json:"note,omitempty" toon:"note,omitempty"`
	Answers  []Answer `json:"answers" toon:"answers"`
}

// --- Stack Exchange API wire shapes (api.stackexchange.com/2.3) ---

// apiWrapper is the common response wrapper every method returns. Items holds
// the method's objects; Backoff, when set, is how many seconds the caller must
// wait before calling the same method again. An error replaces items with
// error_id / error_name / error_message (HTTP 400 for every error).
type apiWrapper[T any] struct {
	Items          []T    `json:"items"`
	HasMore        bool   `json:"has_more"`
	QuotaMax       int    `json:"quota_max"`
	QuotaRemaining int    `json:"quota_remaining"`
	Backoff        int    `json:"backoff"`
	ErrorID        int    `json:"error_id"`
	ErrorName      string `json:"error_name"`
	ErrorMessage   string `json:"error_message"`
}

// apiOwner is a shallow_user. DisplayName is HTML-encoded by the API.
type apiOwner struct {
	DisplayName string `json:"display_name"`
}

// apiQuestion is one item of GET /questions/{ids} with filter=withbody.
type apiQuestion struct {
	QuestionID       int64    `json:"question_id"`
	Title            string   `json:"title"` // HTML-encoded
	Score            int      `json:"score"`
	Tags             []string `json:"tags"`
	Owner            apiOwner `json:"owner"`
	CreationDate     int64    `json:"creation_date"` // unix seconds
	AnswerCount      int      `json:"answer_count"`
	AcceptedAnswerID int64    `json:"accepted_answer_id"`
	ClosedReason     string   `json:"closed_reason"`
	Body             string   `json:"body"` // HTML
}

// apiAnswer is one item of GET /questions/{ids}/answers with filter=withbody,
// and (without the body) of GET /answers/{ids}, which is how a short /a/ link
// finds its question.
type apiAnswer struct {
	AnswerID     int64    `json:"answer_id"`
	QuestionID   int64    `json:"question_id"`
	Score        int      `json:"score"`
	IsAccepted   bool     `json:"is_accepted"`
	Owner        apiOwner `json:"owner"`
	CreationDate int64    `json:"creation_date"`
	Body         string   `json:"body"` // HTML
}
