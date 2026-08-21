package dashboard

import (
	"html"
	"strings"
)

// renderInline turns the inline markdown of one line into HTML.
//
// A scan rather than a chain of regex substitutions, which is what makes a code
// span opaque: its content is escaped and written in one step, so nothing later
// in the line's parse can reach inside it. That is the difference from v1, where
// the substitutions ran over the already-tagged string.
//
// Recursion handles nesting — **bold with `code` inside** — and terminates
// because every recursive call is on a strictly shorter slice.
func renderInline(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)

	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			// A code span ends at the next backtick. Unterminated, it is a
			// literal backtick, which is what a lone ` in prose means.
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString("<code>")
				b.WriteString(html.EscapeString(s[i+1 : i+1+j]))
				b.WriteString("</code>")
				i += j + 2
				continue
			}

		case s[i] == '[':
			if label, href, n, ok := parseLink(s[i:]); ok {
				if href == "" {
					// The target was refused. The label is still text the
					// author wrote, so it renders; only the link is dropped.
					b.WriteString(renderInline(label))
				} else {
					b.WriteString(`<a href="` + href + `" target="_blank" rel="noopener">`)
					b.WriteString(renderInline(label))
					b.WriteString("</a>")
				}
				i += n
				continue
			}

		case strings.HasPrefix(s[i:], "**"):
			if j := strings.Index(s[i+2:], "**"); j > 0 {
				b.WriteString("<strong>" + renderInline(s[i+2:i+2+j]) + "</strong>")
				i += j + 4
				continue
			}

		case strings.HasPrefix(s[i:], "__"):
			if j := findUnderscoreClose(s, i+2, "__"); j > 0 {
				b.WriteString("<strong>" + renderInline(s[i+2:j]) + "</strong>")
				i = j + 2
				continue
			}

		case s[i] == '*':
			if j := strings.IndexByte(s[i+1:], '*'); j > 0 {
				b.WriteString("<em>" + renderInline(s[i+1:i+1+j]) + "</em>")
				i += j + 2
				continue
			}

		case s[i] == '_':
			if j := findUnderscoreClose(s, i+1, "_"); j > 0 {
				b.WriteString("<em>" + renderInline(s[i+1:j]) + "</em>")
				i = j + 1
				continue
			}
		}

		// Nothing matched: one byte of literal text. Escaping byte by byte is
		// safe because only ASCII punctuation is ever replaced and the bytes of
		// a multi-byte rune all pass through untouched.
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&#34;")
		case '\'':
			b.WriteString("&#39;")
		default:
			b.WriteByte(s[i])
		}
		i++
	}
	return b.String()
}

// findUnderscoreClose locates the closing delimiter of an underscore emphasis
// run that opened before from, or 0 if there is none.
//
// The opening underscore has already been accepted by the caller's position in
// the scan; what this adds is the word-boundary rule. Underscore emphasis may
// not begin or end inside a word, so mod_id and Zomboid_1 stay literal — v1
// rendered those as mod<em>id</em> with the underscores eaten, on a page whose
// subject is file names.
//
// The check on the opening side is done here too, by requiring the byte before
// the delimiter not to be a word byte.
func findUnderscoreClose(s string, from int, delim string) int {
	open := from - len(delim)
	if open > 0 && isWordByte(s[open-1]) {
		return 0
	}
	for i := from; i+len(delim) <= len(s); i++ {
		if !strings.HasPrefix(s[i:], delim) {
			continue
		}
		if i == from {
			return 0 // empty content: "__" or "_" doubled
		}
		if end := i + len(delim); end < len(s) && isWordByte(s[end]) {
			continue
		}
		return i
	}
	return 0
}

// isWordByte reports whether b is part of a word for the purpose of the
// underscore rule. Every byte of a multi-byte rune is >= 0x80 and counts, so the
// rule holds for Cyrillic as well as ASCII.
func isWordByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b >= 0x80:
		return true
	}
	return false
}

// parseLink reads a [label](target) starting at s[0], returning the label, an
// attribute-ready href, and how many bytes the whole construct occupied.
//
// A refused target — an unknown scheme — comes back as an empty href with ok
// still true: the construct was consumed and the caller renders the label as
// plain text. Neither the label nor the target may be empty, and the label may
// not contain a ']', which matches v1's regex and keeps nested links out.
func parseLink(s string) (label, href string, n int, ok bool) {
	rb := strings.IndexByte(s, ']')
	if rb < 2 || rb+1 >= len(s) || s[rb+1] != '(' {
		return "", "", 0, false
	}
	paren := strings.IndexByte(s[rb+2:], ')')
	if paren < 1 {
		return "", "", 0, false
	}
	target := s[rb+2 : rb+2+paren]
	return s[1:rb], safeHref(target), rb + 3 + paren, true
}

// safeHref escapes a link target for an attribute, or returns "" if it is not a
// target we are willing to emit.
//
// The list is what a README needs: absolute web and mail links, and anything
// relative or a fragment. Everything else is refused — the case that matters is
// javascript:, which v1 would have emitted verbatim into href, making a link in
// a repository file a script on the public page.
func safeHref(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// A control character inside a URL is either an encoding accident or an
	// attempt to break a scheme check by splitting it.
	if strings.ContainsFunc(u, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	if schemeRe.MatchString(u) {
		scheme := strings.ToLower(u[:strings.IndexByte(u, ':')])
		switch scheme {
		case "http", "https", "mailto":
		default:
			return ""
		}
	}
	return html.EscapeString(u)
}
