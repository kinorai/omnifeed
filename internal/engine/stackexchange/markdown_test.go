package stackexchange

import "testing"

func TestHTMLToMarkdown(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"paragraphs", "<p>One</p>\n<p>Two &amp; three</p>", "One\n\nTwo & three"},
		{"code block keeps content verbatim",
			"<p>Run:</p>\n<pre class=\"lang-cpp prettyprint-override\"><code>#include &lt;vector&gt;\n\nint  main() {}\n</code></pre>\n<p>Done</p>",
			"Run:\n\n```cpp\n#include <vector>\n\nint  main() {}\n```\n\nDone"},
		{"lang-none has no language", "<pre class=\"lang-none\"><code>x</code></pre>", "```\nx\n```"},
		{"no class", "<pre><code>a &lt; b</code></pre>", "```\na < b\n```"},
		{"fence grows past backticks", "<pre><code>```\nnested\n```</code></pre>", "````\n```\nnested\n```\n````"},
		{"inline code", "<p>Use <code>x &gt;= 1</code> here</p>", "Use `x >= 1` here"},
		{"inline code with a backtick", "<p><code>a`b</code></p>", "``a`b``"},
		{"link with text", `<p>See <a href="https://go.dev/doc" rel="nofollow">the docs</a>.</p>`, "See [the docs](https://go.dev/doc)."},
		{"bare link", `<a href="https://go.dev/doc">https://go.dev/doc</a>`, "https://go.dev/doc"},
		{"protocol-relative link", `<a href="//stackoverflow.com/a/1">this</a>`, "[this](https://stackoverflow.com/a/1)"},
		{"root-relative link", `<a href="/questions/2/x">that</a>`, "[that](https://stackoverflow.com/questions/2/x)"},
		{"ampersand in href decoded once", `<a href="https://x.test/?a=1&amp;b=2">q</a>`, "[q](https://x.test/?a=1&b=2)"},
		{"mailto keeps text", `<a href="mailto:a@b.c">mail me</a>`, "mail me"},
		{"image", `<p><img src="https://i.sstatic.net/x.png" alt="graph"></p>`, "![graph](https://i.sstatic.net/x.png)"},
		{"emphasis", "<p><strong>Bold</strong> and <em>it</em> and <del>gone</del></p>", "**Bold** and *it* and ~~gone~~"},
		{"heading", "<h2>What is it?</h2>\n<p>Text</p>", "## What is it?\n\nText"},
		{"unordered list", "<ul>\n<li>a</li>\n<li>b</li>\n</ul>", "- a\n- b"},
		{"ordered list", "<ol>\n<li>first</li>\n<li>second</li>\n</ol>", "1. first\n1. second"},
		{"blockquote", "<blockquote>\n<p>Quoted\nline</p>\n</blockquote>", "> Quoted\n> line"},
		{"table",
			"<table>\n<thead>\n<tr>\n<th>Scenario</th>\n<th>Time <em>(s)</em></th>\n</tr>\n</thead>\n<tbody>\n<tr>\n<td>Random</td>\n<td>a|b</td>\n</tr>\n</tbody>\n</table>",
			"| Scenario | Time *(s)* |\n| --- | --- |\n| Random | a\\|b |"},
		{"rule and break", "<p>a<br>b</p>\n<hr />\n<p>c</p>", "a\nb\n\n---\n\nc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := htmlToMarkdown(tc.in, "stackoverflow.com"); got != tc.want {
				t.Errorf("htmlToMarkdown()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}
