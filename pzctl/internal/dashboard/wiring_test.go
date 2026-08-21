package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// The one seam a Go compiler cannot check: the browser reaches into the rendered
// document by id, and nothing links the two files. A renamed or dropped id makes
// getElementById return null, the next property access throws, and the whole
// script stops — so the poll dies, the language switch dies, and the unlock modal
// stops opening, all with a page that looks fine until someone opens a console.
//
// Both directions are worth checking, but only one of them is an error: an id the
// script wants and no template renders is a broken page, while an id a template
// carries that the script never touches is just a hook for CSS or a test.
func TestEveryScriptedIDExistsInATemplate(t *testing.T) {
	wanted := scriptedIDs(t, "assets/dashboard.js")
	if len(wanted) < 10 {
		t.Fatalf("only %d ids found in the script — the extraction is broken, not the page", len(wanted))
	}

	// The union of both pages, because dashboard.js is served to both and each id
	// only has to exist on the page that uses it. Which page is a runtime concern
	// the script already handles by testing for null on the ones it shares.
	have := map[string]bool{}
	for _, name := range []string{"templates/common.html", "templates/page.html", "templates/backups.html"} {
		for _, id := range templateIDs(t, name) {
			have[id] = true
		}
	}

	for _, id := range wanted {
		if !have[id] {
			t.Errorf("dashboard.js looks up id %q and no template renders it", id)
		}
	}
}

var (
	getByIDRe  = regexp.MustCompile(`getElementById\(\s*"([^"]+)"\s*\)`)
	templIDRe  = regexp.MustCompile(`\bid="([^"{}]+)"`)
	querySelRe = regexp.MustCompile(`querySelector(?:All)?\(\s*"([^"]+)"\s*\)`)
)

func scriptedIDs(t *testing.T, name string) []string {
	t.Helper()
	b, err := files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range getByIDRe.FindAllStringSubmatch(string(b), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func templateIDs(t *testing.T, name string) []string {
	t.Helper()
	b, err := files.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range templIDRe.FindAllStringSubmatch(string(b), -1) {
		out = append(out, m[1])
	}
	return out
}

// The class selectors the script reaches for, checked the same way. This is the
// looser half — a selector can legitimately match markup the guide's rendered
// markdown produced — so only the ones the script itself introduces are listed,
// and the assertion is that the class appears somewhere in the templates.
func TestEveryScriptedClassSelectorAppearsInATemplate(t *testing.T) {
	b, err := files.ReadFile("assets/dashboard.js")
	if err != nil {
		t.Fatal(err)
	}
	markup := ""
	for _, name := range []string{"templates/common.html", "templates/page.html", "templates/backups.html"} {
		h, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		markup += string(h)
	}

	for _, m := range querySelRe.FindAllStringSubmatch(string(b), -1) {
		sel := m[1]
		if !strings.HasPrefix(sel, ".") {
			continue // an element or attribute selector; not ours to verify here
		}
		if !strings.Contains(markup, strings.TrimPrefix(sel, ".")) {
			t.Errorf("dashboard.js selects %q and no template contains that class", sel)
		}
	}
}
