// Markdown rendering for the installation guide.
//
// The guide is a file in the pz-saves repository (README.ru.md / README.en.md),
// so the dashboard has to turn markdown into HTML. v1 did this with
// markdown_to_html — 110 lines of line-oriented Python, a subset covering
// headings, fenced code, blockquotes, rules, both list kinds, and five inline
// forms. That subset is exactly right for the document, and this is a port of
// it: same blocks, same inline forms, same output tags, so the rendered guide
// diffs clean against the running page.
//
// It is not a transcription. v1 built HTML by regex substitution over a string
// it had already escaped, which leaks in three places:
//
//   - The link target went into href="..." with only &<> escaped, so a target
//     containing a double quote escapes the attribute, and one beginning
//     "javascript:" is a live script. Here the target is quote-escaped and its
//     scheme is checked against a list.
//   - The fence's info string went into class="code-block {lang}" unescaped.
//     Here it is filtered to an identifier.
//   - The inline regexes ran over the whole line after code spans had already
//     become <code> tags, so markup inside a code span was still substituted:
//     `a**b**c` rendered as <code>a<strong>b</strong>c</code>. Here inline
//     parsing is a scan, and a code span is opaque to everything after it.
//
// Two further deviations, both improvements, both visible:
//
//   - v1 treated any _ as emphasis, so a path like Zomboid_1 or a name like
//     mod_id rendered with half of it italic and the underscores eaten. Here _
//     opens emphasis only at a word boundary, which is CommonMark's rule and
//     the reason CommonMark has it. * keeps v1's laxer behaviour.
//   - v1 returned the English sentence "No installation instructions provided."
//     for an empty document — a hardcoded, unlocalised string. Here an empty
//     document renders as nothing and the caller omits the section.
package dashboard

import (
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"
)

var (
	ruleRe   = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	olRe     = regexp.MustCompile(`^(\d+)\.\s+(.*)$`)
	langRe   = regexp.MustCompile(`[^A-Za-z0-9_+-]`)
	schemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// RenderMarkdown converts the guide's markdown to HTML.
//
// The result is template.HTML because it is markup by construction and the
// template must not escape it again. Everything that came from the document is
// escaped on the way in; nothing reaches the output unescaped except the tags
// this function itself emits.
//
// An empty or whitespace-only document yields "", which callers test for. The
// alternative — a placeholder sentence — is a string in a language the page may
// not be rendering.
func RenderMarkdown(md string) template.HTML {
	if strings.TrimSpace(md) == "" {
		return ""
	}

	var out []string
	var inCode, inUL, inOL bool

	// closeLists ends whichever list is open. Every block that is not a list
	// item calls this first: markdown has no way to write "</ul>", so the only
	// signal is the next line not being an item.
	closeLists := func() {
		if inUL {
			out = append(out, "</ul>")
			inUL = false
		}
		if inOL {
			out = append(out, "</ol>")
			inOL = false
		}
	}

	for _, raw := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		stripped := strings.TrimSpace(line)

		// A fence toggles. Indented fences count, which is what lets a fenced
		// block sit inside a list in the source without the parser noticing.
		if strings.HasPrefix(stripped, "```") {
			if inCode {
				out = append(out, "</code></pre>")
				inCode = false
			} else {
				closeLists()
				lang := langRe.ReplaceAllString(strings.TrimSpace(stripped[3:]), "")
				out = append(out, fmt.Sprintf(`<pre class="code-block %s"><code>`, lang))
				inCode = true
			}
			continue
		}
		if inCode {
			// Indentation is content inside a fence, so this uses line, not
			// stripped. No inline parsing: a code block is literal.
			out = append(out, html.EscapeString(line))
			continue
		}

		if stripped == "" {
			closeLists()
			continue
		}

		switch {
		case strings.HasPrefix(stripped, "### "):
			closeLists()
			out = append(out, "<h3>"+renderInline(stripped[4:])+"</h3>")
		case strings.HasPrefix(stripped, "## "):
			closeLists()
			out = append(out, "<h2>"+renderInline(stripped[3:])+"</h2>")
		case strings.HasPrefix(stripped, "# "):
			closeLists()
			out = append(out, "<h1>"+renderInline(stripped[2:])+"</h1>")
		case strings.HasPrefix(stripped, "> "):
			closeLists()
			out = append(out, "<blockquote>"+renderInline(stripped[2:])+"</blockquote>")
		case ruleRe.MatchString(stripped):
			closeLists()
			out = append(out, "<hr>")
		case strings.HasPrefix(stripped, "- "), strings.HasPrefix(stripped, "* "), strings.HasPrefix(stripped, "+ "):
			if inOL {
				out = append(out, "</ol>")
				inOL = false
			}
			if !inUL {
				out = append(out, "<ul>")
				inUL = true
			}
			out = append(out, "<li>"+renderInline(stripped[2:])+"</li>")
		default:
			if m := olRe.FindStringSubmatch(stripped); m != nil {
				if inUL {
					out = append(out, "</ul>")
					inUL = false
				}
				if !inOL {
					out = append(out, "<ol>")
					inOL = true
				}
				out = append(out, "<li>"+renderInline(m[2])+"</li>")
				break
			}
			closeLists()
			out = append(out, "<p>"+renderInline(stripped)+"</p>")
		}
	}

	// An unterminated fence or a document ending inside a list still has to
	// produce balanced markup: this is a fragment interpolated into a page, and
	// a stray open <ul> would swallow the rest of it.
	if inCode {
		out = append(out, "</code></pre>")
	}
	if inUL {
		out = append(out, "</ul>")
	}
	if inOL {
		out = append(out, "</ol>")
	}

	return template.HTML(strings.Join(out, "\n"))
}
