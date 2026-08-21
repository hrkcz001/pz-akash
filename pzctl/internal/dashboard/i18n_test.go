package dashboard

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseLang(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Lang
		ok   bool
	}{
		{"ru", RU, true},
		{"en", EN, true},
		{"RU", RU, true},
		{"  en  ", EN, true},

		// Accept-Language forms. A browser sends "ru-RU,ru;q=0.9,en;q=0.8"; the
		// caller splits on the comma, and what reaches us still has the region
		// and the quality factor attached.
		{"ru-RU", RU, true},
		{"en-GB", EN, true},
		{"en_US", EN, true},
		{"ru;q=0.9", RU, true},

		{"", "", false},
		{"de", "", false},
		{"ruq", "", false},
		{"russian", "", false},

		// A leading separator must not strip the whole name into "", which would
		// then match nothing but is worth pinning: IndexAny > 0, not >= 0.
		{"-ru", "", false},
	} {
		got, ok := ParseLang(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseLang(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPluralizeRussian(t *testing.T) {
	p := Plural{One: "файл", Few: "файла", Many: "файлов"}
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "файлов"},
		{1, "файл"},
		{2, "файла"},
		{3, "файла"},
		{4, "файла"},
		{5, "файлов"},
		{10, "файлов"},

		// The teens are the whole reason the rule is not "n == 1". 11 ends in a
		// 1 and 12 ends in a 2, but both take "many".
		{11, "файлов"},
		{12, "файлов"},
		{14, "файлов"},
		{19, "файлов"},
		{20, "файлов"},

		// Past the teens the low digits govern again.
		{21, "файл"},
		{22, "файла"},
		{25, "файлов"},

		{101, "файл"},
		{102, "файла"},
		{111, "файлов"},
		{112, "файлов"},
		{121, "файл"},

		{1000, "файлов"},
		{1001, "файл"},
	} {
		if got := RU.pluralize(tc.n, p); got != tc.want {
			t.Errorf("RU.pluralize(%d) = %q; want %q", tc.n, got, tc.want)
		}
	}
}

func TestPluralizeEnglish(t *testing.T) {
	p := Plural{One: "file", Many: "files"}
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "files"},
		{1, "file"},
		{2, "files"},
		{11, "files"},
		{21, "files"},
		{101, "files"},
	} {
		if got := EN.pluralize(tc.n, p); got != tc.want {
			t.Errorf("EN.pluralize(%d) = %q; want %q", tc.n, got, tc.want)
		}
	}
}

// A negative count is not a case the page can reach — state.PlayersUnknown is
// -1 and the view renders PlayersUnknown instead of a number — but pluralize is
// the kind of helper that acquires new callers, and "-1 игрокf" is a worse
// failure than a wrong-but-grammatical form.
func TestPluralizeNegative(t *testing.T) {
	p := Plural{One: "игрок", Few: "игрока", Many: "игроков"}
	if got, want := RU.pluralize(-1, p), "игрок"; got != want {
		t.Errorf("RU.pluralize(-1) = %q; want %q", got, want)
	}
	if got, want := RU.pluralize(-3, p), "игрока"; got != want {
		t.Errorf("RU.pluralize(-3) = %q; want %q", got, want)
	}
}

// An unknown locale is treated as English rather than panicking or emitting the
// Russian forms, because pluralize is reached from a template where neither of
// those has a recovery.
func TestPluralizeUnknownLocaleFallsBackToEnglishRule(t *testing.T) {
	p := Plural{One: "mod", Many: "mods"}
	if got, want := Lang("de").pluralize(2, p), "mods"; got != want {
		t.Errorf("de.pluralize(2) = %q; want %q", got, want)
	}
}

// The struct-literal catalog makes a *missing field* a build failure, which is
// most of the point of using a struct. It does not make an *empty* field one:
// omitting a line from one locale's literal still compiles, and the page then
// renders a blank where a button's label belongs. This closes that gap.
func TestCatalogIsComplete(t *testing.T) {
	if len(catalog) != len(Langs) {
		t.Fatalf("catalog has %d locales, Langs has %d", len(catalog), len(Langs))
	}
	for _, lang := range Langs {
		msgs, ok := catalog[lang]
		if !ok {
			t.Errorf("no catalog entry for %q", lang)
			continue
		}
		v := reflect.ValueOf(msgs)
		typ := v.Type()
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			switch f := v.Field(i); f.Kind() {
			case reflect.String:
				if strings.TrimSpace(f.String()) == "" {
					t.Errorf("%s.%s is empty", lang, name)
				}
			case reflect.Struct: // Plural
				p := f.Interface().(Plural)
				if strings.TrimSpace(p.One) == "" {
					t.Errorf("%s.%s.One is empty", lang, name)
				}
				if strings.TrimSpace(p.Many) == "" {
					t.Errorf("%s.%s.Many is empty", lang, name)
				}
				// Few is Russian-only: English legitimately leaves it unset, and
				// pluralize never reads it for a non-Russian locale.
				if lang == RU && strings.TrimSpace(p.Few) == "" {
					t.Errorf("%s.%s.Few is empty", lang, name)
				}
			default:
				t.Errorf("%s.%s has unexpected kind %s; extend this test", lang, name, f.Kind())
			}
		}
	}
}

// DiskWarning is the one message carrying a format verb. If a locale loses the
// %d, the page prints the sentence without the number that makes it actionable;
// if it gains a second verb, fmt appends "%!d(MISSING)" to a player-facing page.
func TestDiskWarningTakesExactlyOneIntVerb(t *testing.T) {
	for _, lang := range Langs {
		got := catalog[lang].DiskWarning
		if n := strings.Count(got, "%d"); n != 1 {
			t.Errorf("%s.DiskWarning has %d %%d verbs, want 1: %q", lang, n, got)
		}
		// %% is the escaped literal percent that follows the number.
		if n := strings.Count(strings.ReplaceAll(got, "%%", ""), "%"); n != 1 {
			t.Errorf("%s.DiskWarning has a verb other than %%d: %q", lang, got)
		}
	}
}
