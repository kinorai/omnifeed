package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

const (
	discussionComments = 100 // top-level comments fetched (GraphQL's page maximum)
	discussionReplies  = 50  // replies fetched per comment
)

// discussionQuery fetches a discussion with its first page of comments and
// replies in one GraphQL call. Discussions have no REST endpoint for
// comments, and GraphQL rejects anonymous callers — hence the token gate in
// Matches.
var discussionQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    discussion(number: $number) {
      title body createdAt upvoteCount
      author { login }
      category { name }
      answer { id }
      comments(first: ` + strconv.Itoa(discussionComments) + `) {
        totalCount
        nodes {
          id body createdAt upvoteCount
          author { login }
          replies(first: ` + strconv.Itoa(discussionReplies) + `) {
            totalCount
            nodes { body createdAt author { login } }
          }
        }
      }
    }
  }
}`

// crawlDiscussion renders a discussion: header, body, accepted answer, then
// every comment with its replies.
func (e *Engine) crawlDiscussion(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	number, _ := strconv.Atoi(t.number) // the URL pattern guarantees digits
	var data struct {
		Repository *struct {
			Discussion *gqlDiscussion `json:"discussion"`
		} `json:"repository"`
	}
	if err := e.graphql(ctx, discussionQuery, map[string]any{"owner": t.owner, "name": t.repo, "number": number}, &data); err != nil {
		return domain.Document{}, fmt.Errorf("fetch discussion: %w", err)
	}
	if data.Repository == nil || data.Repository.Discussion == nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindError, StatusCode: http.StatusNotFound,
			Err: errors.New("discussion not found")}
	}
	d := data.Repository.Discussion

	answerID := ""
	if d.Answer != nil {
		answerID = d.Answer.ID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", oneLine(d.Title))
	fmt.Fprintf(&b, "- Discussion: %s/%s#%s, category: %s\n", t.owner, t.repo, t.number, d.Category.Name)
	fmt.Fprintf(&b, "- Author: %s, %s, %s\n", d.Author.name(), day(d.CreatedAt), plural(d.UpvoteCount, "upvote", "upvotes"))
	fmt.Fprintf(&b, "- Answered: %s\n", map[bool]string{true: "yes", false: "no"}[answerID != ""])
	fmt.Fprintf(&b, "- Comments: %d\n", d.Comments.TotalCount)
	if body := strings.TrimSpace(d.Body); body != "" {
		b.WriteString("\n" + demoteHeadings(body, 2) + "\n")
	}

	// The accepted answer goes first, so a reader who stops early still has it.
	for _, c := range d.Comments.Nodes {
		if c.ID == answerID {
			fmt.Fprintf(&b, "\n## Accepted answer, by %s (%s)\n\n%s\n", c.Author.name(), day(c.CreatedAt),
				demoteHeadings(strings.TrimSpace(c.Body), 3))
		}
	}

	var notes []string
	if len(d.Comments.Nodes) > 0 {
		b.WriteString("\n## Comments\n")
	}
	for _, c := range d.Comments.Nodes {
		label := fmt.Sprintf("%s (%s, %s)", c.Author.name(), day(c.CreatedAt), plural(c.UpvoteCount, "upvote", "upvotes"))
		if c.ID == answerID {
			fmt.Fprintf(&b, "\n### %s: accepted answer, shown above\n", label)
		} else {
			fmt.Fprintf(&b, "\n### %s\n\n%s\n", label, demoteHeadings(strings.TrimSpace(c.Body), 4))
		}
		for _, r := range c.Replies.Nodes {
			fmt.Fprintf(&b, "\n#### Reply by %s (%s)\n\n%s\n", r.Author.name(), day(r.CreatedAt),
				demoteHeadings(strings.TrimSpace(r.Body), 5))
		}
		if c.Replies.TotalCount > len(c.Replies.Nodes) {
			notes = append(notes, fmt.Sprintf("a comment by %s shows %d of %d replies",
				c.Author.name(), len(c.Replies.Nodes), c.Replies.TotalCount))
		}
	}
	meta := map[string]string{"github_kind": kindNames[t.kind], "comments": strconv.Itoa(len(d.Comments.Nodes))}
	if d.Comments.TotalCount > len(d.Comments.Nodes) {
		meta["truncated_from"] = strconv.Itoa(d.Comments.TotalCount)
		notes = append([]string{fmt.Sprintf("showing %d of %d comments", len(d.Comments.Nodes), d.Comments.TotalCount)}, notes...)
	}
	if len(notes) > 0 {
		b.WriteString("\n[Truncated: " + strings.Join(notes, "; ") + ".]\n")
	}
	return e.markdownDocument(b.String(), rawURL, meta), nil
}

// graphql runs one GraphQL query against {apiBase}/graphql and decodes its
// data into out. GraphQL reports failures (not found, bad query) as a 200
// with an errors array, which is surfaced as an error here.
func (e *Engine) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	reqBody, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	raw, _, err := e.do(ctx, http.MethodPost, e.apiBase+"/graphql", reqBody, "application/json")
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if jerr := json.Unmarshal(raw, &resp); jerr != nil {
		return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse graphql response: %w", jerr)}
	}
	if len(resp.Errors) > 0 {
		fe := &domain.FetchError{Kind: domain.KindError, Err: fmt.Errorf("graphql: %s", resp.Errors[0].Message)}
		if resp.Errors[0].Type == "NOT_FOUND" {
			fe.StatusCode = http.StatusNotFound
		}
		return fe
	}
	if jerr := json.Unmarshal(resp.Data, out); jerr != nil {
		return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse graphql data: %w", jerr)}
	}
	return nil
}
