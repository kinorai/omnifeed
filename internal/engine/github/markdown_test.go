package github

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Relative links resolve the way GitHub renders them: pages to the blob view,
// images to raw.githubusercontent.com; absolute URLs and anchors are kept.
func TestResolveLinks(t *testing.T) {
	rl := repoLinks{owner: "o", repo: "r", ref: "main", dir: "docs"}
	for in, want := range map[string]string{
		"install.md":            "https://github.com/o/r/blob/main/docs/install.md",
		"./install.md#setup":    "https://github.com/o/r/blob/main/docs/install.md#setup",
		"../README.md":          "https://github.com/o/r/blob/main/README.md",
		"/chart":                "https://github.com/o/r/blob/main/chart",
		"img/arch.PNG":          "https://raw.githubusercontent.com/o/r/main/docs/img/arch.PNG",
		"https://example.com/x": "https://example.com/x",
		"mailto:a@b.c":          "mailto:a@b.c",
		"//cdn.example.com/x":   "//cdn.example.com/x",
		"#anchor":               "#anchor",
	} {
		if got := rl.resolve(in); got != want {
			t.Errorf("resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanMarkdown(t *testing.T) {
	in := strings.Join([]string{
		`<!-- hidden -->`,
		`<h1 align="center"><a href="https://x.io/"><img src="logo.png"></a></h1>`,
		`<h2>Real <b>title</b></h2>`,
		`<p align="center">Visit <a href="docs/x.md" target="_blank">the docs</a>.</p>`,
		`[![CI](https://github.com/o/r/actions/workflows/ci.yml/badge.svg)](https://github.com/o/r/actions)`,
		`![shot](shot.png)`,
		`* Tool: [![Report](https://goreportcard.com/badge/x)](https://goreportcard.com/report/x) see [ref][r]`,
		``, ``, ``,
		`[r]: CONTRIBUTING.md`,
		`[^1]: A footnote.`,
		"```html",
		`<p><img src="kept.png"></p> [kept](kept.md)`,
		"```",
	}, "\n")
	got := cleanMarkdown(in, repoLinks{owner: "o", repo: "r", ref: "main"})
	for _, want := range []string{
		"## Real title",
		"Visit [the docs](https://github.com/o/r/blob/main/docs/x.md).",
		"![shot](https://raw.githubusercontent.com/o/r/main/shot.png)", // a screenshot, not a badge
		"* Tool:  see [ref][r]",
		"[r]: https://github.com/o/r/blob/main/CONTRIBUTING.md",
		"[^1]: A footnote.",                                           // a footnote, not a link definition
		"```html\n<p><img src=\"kept.png\"></p> [kept](kept.md)\n```", // code untouched
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"hidden", "logo.png", "badge.svg", "goreportcard", "<p", "\n\n\n"} {
		if strings.Contains(strings.Split(got, "```html")[0], unwanted) {
			t.Errorf("unexpected %q in:\n%s", unwanted, got)
		}
	}
	if strings.HasPrefix(got, "#") && !strings.HasPrefix(got, "## Real title") {
		t.Errorf("empty logo heading kept:\n%s", got)
	}
}

func TestDemoteHeadings(t *testing.T) {
	in := "# A\ntext #notheading\n#### B\n##### C\n```\n# code\n```\n"
	want := "### A\ntext #notheading\n###### B\n###### C\n```\n# code\n```\n"
	if got := demoteHeadings(in, 2); got != want {
		t.Errorf("demoteHeadings = %q, want %q", got, want)
	}
}

// excerpt cuts at a line boundary and closes a code fence the cut left open.
func TestExcerpt(t *testing.T) {
	if got, cut := excerpt("short\r\nnotes", 100); got != "short\nnotes" || cut {
		t.Errorf("short input = %q, %v", got, cut)
	}
	if got, _ := excerpt(strings.Repeat("é", 50), 15); !utf8.ValidString(got) {
		t.Errorf("excerpt split a character: %q", got)
	}
	in := "intro line here\n```\n" + strings.Repeat("code line\n", 20) + "```\nafter"
	got, cut := excerpt(in, 80)
	if !cut || !strings.HasSuffix(got, "code line\n```") {
		t.Errorf("excerpt = %q, %v; want a cut closed with a fence", got, cut)
	}
}

// fenced picks a fence longer than any backtick run in the content.
func TestFenced(t *testing.T) {
	if got := fenced("a\n", "go"); got != "```go\na\n```" {
		t.Errorf("fenced = %q", got)
	}
	if got := fenced("x ``` y", ""); got != "````\nx ``` y\n````" {
		t.Errorf("fenced with backticks = %q", got)
	}
}

func TestCodeLang(t *testing.T) {
	for name, want := range map[string]string{
		"main.go": "go", "x/y.py": "python", "Dockerfile": "dockerfile", "Makefile": "makefile",
		"values.yml": "yaml", "a.tf": "hcl", "LICENSE": "", "weird.zig": "zig",
	} {
		if got := codeLang(name); got != want {
			t.Errorf("codeLang(%q) = %q, want %q", name, got, want)
		}
	}
}

// A multi-byte character split by the binary-sniff window is still text.
func TestIsBinary(t *testing.T) {
	text := []byte(strings.Repeat("a", binarySniff-1) + "é") // é straddles the window
	if isBinary(text) {
		t.Error("UTF-8 text split at the sniff window reported binary")
	}
	if !isBinary([]byte("PNG\x00data")) {
		t.Error("NUL byte not reported binary")
	}
	if !isBinary([]byte{0xff, 0xfe, 'a'}) {
		t.Error("invalid UTF-8 not reported binary")
	}
}
