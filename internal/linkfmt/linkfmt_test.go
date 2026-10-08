package linkfmt

import "testing"

func TestLink(t *testing.T) {
	cases := []struct{ name, text, href, want string }{
		{"text equals href", "https://go.dev/doc", "https://go.dev/doc", "https://go.dev/doc"},
		{"text is a prefix of href", "https://go.dev/doc", "https://go.dev/doc/faq", "https://go.dev/doc/faq"},
		{"ascii ellipsis", "https://example.com/a/ve...", "https://example.com/a/very/long", "https://example.com/a/very/long"},
		{"unicode ellipsis", "example.com/a/very…", "https://example.com/a/very/long", "https://example.com/a/very/long"},
		{"schemeless prefix", "example.com/a", "https://example.com/a", "https://example.com/a"},
		{"www stripped prefix", "example.com", "http://www.example.com/", "http://www.example.com/"},
		{"custom text", "the Go blog", "https://go.dev/blog", "[the Go blog](https://go.dev/blog)"},
		{"custom text trimmed", "  the blog ", "https://go.dev/blog", "[the blog](https://go.dev/blog)"},
		{"empty text", "", "https://go.dev/", "https://go.dev/"},
		{"empty href", "plain", "", "plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Link(tc.text, tc.href); got != tc.want {
				t.Errorf("Link(%q, %q) = %q, want %q", tc.text, tc.href, got, tc.want)
			}
		})
	}
}
