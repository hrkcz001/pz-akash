package dashboard

import (
	"strings"
	"testing"
)

func TestRenderMarkdownBlocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "  \n\t\n  ", ""},

		{"paragraph", "hello", "<p>hello</p>"},
		{"headings", "# One\n## Two\n### Three", "<h1>One</h1>\n<h2>Two</h2>\n<h3>Three</h3>"},

		// A heading needs the space. "#tag" is prose.
		{"hash without space is prose", "#tag", "<p>#tag</p>"},

		{"blockquote", "> note", "<blockquote>note</blockquote>"},
		{"rule dashes", "---", "<hr>"},
		{"rule stars", "***", "<hr>"},
		{"rule underscores", "___", "<hr>"},

		{
			"unordered list of three markers",
			"- a\n* b\n+ c",
			"<ul>\n<li>a</li>\n<li>b</li>\n<li>c</li>\n</ul>",
		},
		{
			"ordered list keeps its own numbering out of the text",
			"1. first\n2. second",
			"<ol>\n<li>first</li>\n<li>second</li>\n</ol>",
		},

		// A blank line is the only thing that ends a list, so this is the case
		// that decides whether the guide's sections nest into each other.
		{
			"blank line closes a list",
			"- a\n\nafter",
			"<ul>\n<li>a</li>\n</ul>\n<p>after</p>",
		},
		{
			"a heading closes a list without a blank line",
			"- a\n## H",
			"<ul>\n<li>a</li>\n</ul>\n<h2>H</h2>",
		},
		{
			"switching list kind closes the first",
			"- a\n1. b",
			"<ul>\n<li>a</li>\n</ul>\n<ol>\n<li>b</li>\n</ol>",
		},
		{
			"a document ending inside a list still closes it",
			"- a",
			"<ul>\n<li>a</li>\n</ul>",
		},

		{
			"fenced block is literal",
			"```\n  indented **not bold** <b>\n```",
			"<pre class=\"code-block \"><code>\n  indented **not bold** &lt;b&gt;\n</code></pre>",
		},
		{
			"fence info string becomes a class",
			"```bash\nls\n```",
			"<pre class=\"code-block bash\"><code>\nls\n</code></pre>",
		},
		{
			"an unterminated fence is closed anyway",
			"```\nls",
			"<pre class=\"code-block \"><code>\nls\n</code></pre>",
		},
		{
			"a fence closes an open list",
			"- a\n```\nx\n```",
			"<ul>\n<li>a</li>\n</ul>\n<pre class=\"code-block \"><code>\nx\n</code></pre>",
		},

		{"crlf is normalised", "a\r\nb", "<p>a</p>\n<p>b</p>"},
	} {
		if got := string(RenderMarkdown(tc.in)); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

// The info string lands in a class attribute. v1 interpolated it unescaped.
func TestFenceInfoStringIsFilteredToAnIdentifier(t *testing.T) {
	got := string(RenderMarkdown("```\" onload=\"alert(1)\nx\n```"))
	want := "<pre class=\"code-block onloadalert1\"><code>\nx\n</code></pre>"
	if got != want {
		t.Errorf("info string reached the attribute:\n got %q\nwant %q", got, want)
	}
}

func TestRenderInline(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"strong stars", "**b**", "<strong>b</strong>"},
		{"strong underscores", "__b__", "<strong>b</strong>"},
		{"em stars", "*i*", "<em>i</em>"},
		{"em underscores", "_i_", "<em>i</em>"},
		{"code", "`x`", "<code>x</code>"},

		{"strong wins over em", "**b** and *i*", "<strong>b</strong> and <em>i</em>"},
		{"nesting", "**b with `c` in**", "<strong>b with <code>c</code> in</strong>"},

		// The v1 bug this port fixes: markup inside a code span stayed markup,
		// because the regexes ran after the <code> tags existed.
		{"code span is opaque", "`a**b**c`", "<code>a**b**c</code>"},
		{"code span keeps underscores", "`Zomboid_1`", "<code>Zomboid_1</code>"},

		// The other v1 bug: intraword underscores. Both of these are file and
		// mod names from the guide's actual subject matter.
		{"intraword underscore is literal", "mod_id", "mod_id"},
		{"trailing underscore run is literal", "Zomboid_1_2", "Zomboid_1_2"},
		{"underscore emphasis at a word boundary still works", "an _italic_ word", "an <em>italic</em> word"},
		{"underscore after punctuation opens", "(_i_)", "(<em>i</em>)"},

		// Cyrillic: every byte is >= 0x80, so the word rule has to hold there
		// too or the Russian guide loses its underscores.
		{"intraword underscore in cyrillic", "мод_ид", "мод_ид"},
		{"cyrillic emphasis", "это _важно_ здесь", "это <em>важно</em> здесь"},

		{"unterminated code is literal", "a ` b", "a ` b"},
		{"unterminated strong is literal", "a ** b", "a ** b"},
		{"empty emphasis is literal", "____", "____"},

		{"escapes angle brackets", "a <b> c", "a &lt;b&gt; c"},
		{"escapes ampersand", "a & b", "a &amp; b"},
		{"escapes quotes", `he said "hi" it's`, "he said &#34;hi&#34; it&#39;s"},
		{"escapes inside code", "`<b>&`", "<code>&lt;b&gt;&amp;</code>"},

		{
			"link",
			"see [docs](https://example.com/a)",
			`see <a href="https://example.com/a" target="_blank" rel="noopener">docs</a>`,
		},
		{
			"relative link",
			"[here](guide.md)",
			`<a href="guide.md" target="_blank" rel="noopener">here</a>`,
		},
		{
			"fragment link",
			"[here](#top)",
			`<a href="#top" target="_blank" rel="noopener">here</a>`,
		},
		{
			"mailto link",
			"[mail](mailto:a@b.c)",
			`<a href="mailto:a@b.c" target="_blank" rel="noopener">mail</a>`,
		},
		{
			"markup in a link label",
			"[**bold**](https://e.com)",
			`<a href="https://e.com" target="_blank" rel="noopener"><strong>bold</strong></a>`,
		},
		{
			"query string ampersand is escaped for the attribute",
			"[q](https://e.com/?a=1&b=2)",
			`<a href="https://e.com/?a=1&amp;b=2" target="_blank" rel="noopener">q</a>`,
		},
		{"bracket that is not a link", "[a] b", "[a] b"},
		{"empty label is not a link", "[](x)", "[](x)"},
		{"empty target is not a link", "[a]()", "[a]()"},
	} {
		if got := renderInline(tc.in); got != tc.want {
			t.Errorf("%s: renderInline(%q)\n got %q\nwant %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// The href hole in v1: the target was escaped for &<> only and its scheme was
// never looked at, so a README — a file in a git repository several people can
// push to — could put a script or an attribute break on the public page.
func TestLinkTargetsThatMustNotSurvive(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"javascript scheme", "[x](javascript:alert(1))"},
		{"javascript with mixed case", "[x](JavaScript:alert(1))"},
		{"data url", "[x](data:text/html;base64,PHNjcmlwdD4=)"},
		{"vbscript", "[x](vbscript:msgbox)"},
		{"scheme split by a newline", "[x](java\nscript:alert(1))"},
		{"scheme split by a tab", "[x](java\tscript:alert(1))"},
	} {
		got := renderInline(tc.in)
		if strings.Contains(got, "<a ") || strings.Contains(got, "href") {
			t.Errorf("%s: emitted a link: %q", tc.name, got)
		}
		// The label survives as text — dropping the author's words would be a
		// worse failure than dropping the link.
		if !strings.Contains(got, "x") {
			t.Errorf("%s: dropped the label too: %q", tc.name, got)
		}
	}
}

func TestLinkTargetCannotEscapeTheAttribute(t *testing.T) {
	got := renderInline(`[x](https://e.com" onmouseover="alert(1))`)
	if strings.Contains(got, `onmouseover="`) {
		t.Errorf("target broke out of href: %q", got)
	}
	if n := strings.Count(got, `"`); n != 6 {
		// href="…" target="_blank" rel="noopener" — six quotes, no more.
		t.Errorf("unbalanced quoting (%d quotes): %q", n, got)
	}
}

// The guide is a real document; this is a fragment shaped like one, checked end
// to end so a change in block/inline interaction shows up as one failure.
func TestRenderMarkdownDocument(t *testing.T) {
	in := strings.Join([]string{
		"## Установка",
		"",
		"1. Удалите папку `Zomboid`.",
		"2. Распакуйте **client.zip** в _папку игры_.",
		"",
		"> Внимание: mod_id менять не нужно.",
		"",
		"```bash",
		"cd ~/Zomboid && ls",
		"```",
		"",
		"Подробнее — [вики](https://pzwiki.net/wiki/Mods).",
	}, "\n")

	want := strings.Join([]string{
		"<h2>Установка</h2>",
		"<ol>",
		"<li>Удалите папку <code>Zomboid</code>.</li>",
		"<li>Распакуйте <strong>client.zip</strong> в <em>папку игры</em>.</li>",
		"</ol>",
		"<blockquote>Внимание: mod_id менять не нужно.</blockquote>",
		`<pre class="code-block bash"><code>`,
		"cd ~/Zomboid &amp;&amp; ls",
		"</code></pre>",
		`<p>Подробнее — <a href="https://pzwiki.net/wiki/Mods" target="_blank" rel="noopener">вики</a>.</p>`,
	}, "\n")

	if got := string(RenderMarkdown(in)); got != want {
		t.Errorf("document:\n got %q\nwant %q", got, want)
	}
}
