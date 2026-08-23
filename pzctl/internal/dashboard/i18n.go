// Package dashboard renders the operator and player facing pages.
//
// v1 built these as Python f-strings — one 1500-line expression for the hub and
// another 360 for the backups table — and then localised them in the browser: the
// server always emitted Russian, and 400 lines of JavaScript walked the DOM by
// element id replacing every string when you clicked EN. That arrangement has
// three costs this package is written to avoid.
//
// The strings could not be checked. `i18nData.en.card_client_btn` is a lookup
// that yields `undefined` when the key is missing or misspelled, and `undefined`
// renders as the word "undefined" on the page. Here a locale is a struct literal,
// so a missing message is a build failure.
//
// The plural rules were written four times. Russian needs three forms and the
// rule is the same for players, mods, files and archives, but v1 had it inline at
// four sites in JavaScript and three more in Python — and every one of them was
// the same seven lines re-typed. Here it exists once, and it is tested.
//
// Nothing was escaped. The page interpolated values into HTML with `{}`, so a
// backup filename or a server name containing a `<` produced broken markup at
// best. html/template escapes by context, which is the entire reason to use it.
package dashboard

import "strings"

// Lang is a locale this package can render. The set is closed: a request for
// anything else falls back to the configured default rather than serving a page
// with holes in it.
type Lang string

const (
	RU Lang = "ru"
	EN Lang = "en"
)

// Langs is every locale, in the order the switcher shows them.
var Langs = []Lang{RU, EN}

// ParseLang resolves a locale name, reporting whether it is one we render.
//
// It accepts the prefix form too ("ru-RU", "en-GB"), because that is what an
// Accept-Language header carries and refusing it would send a Russian browser to
// the fallback for a locale we have.
func ParseLang(s string) (Lang, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, "-_;"); i > 0 {
		s = s[:i]
	}
	for _, l := range Langs {
		if string(l) == s {
			return l, true
		}
	}
	return "", false
}

// Plural holds the forms of one countable noun.
//
// English uses One and Many. Russian uses all three, and which one applies is
// not a matter of "singular or not": 1 file is "файл", 2 are "файла", 5 are
// "файлов", and 11 are "файлов" again while 21 is back to "файл".
type Plural struct {
	One  string // 1, 21, 101
	Few  string // 2-4, 22-24
	Many string // 0, 5-20, 11-19
}

// pluralize picks the form of p that agrees with n.
//
// The Russian rule is CLDR's, which is also what v1 implemented — correctly, at
// seven separate sites. The n%100 in 11..19 test has to come first, or 11 takes
// the "one" branch that 21 legitimately wants.
func (l Lang) pluralize(n int, p Plural) string {
	if n < 0 {
		n = -n
	}
	if l != RU {
		if n == 1 {
			return p.One
		}
		return p.Many
	}
	switch mod100, mod10 := n%100, n%10; {
	case mod100 >= 11 && mod100 <= 19:
		return p.Many
	case mod10 == 1:
		return p.One
	case mod10 >= 2 && mod10 <= 4:
		return p.Few
	default:
		return p.Many
	}
}

// text is the message catalog for one locale.
//
// A struct rather than a map, so that adding a message obliges every locale to
// carry it. The alternative — a map with a lookup helper — is what v1's
// JavaScript did, and it fails silently in the one direction that matters: a
// missing translation renders as nothing, or as the literal "undefined", on a
// page nobody is checking word by word.
type text struct {
	// Chrome
	PageTitle  string
	Brand      string
	NavConnect string
	NavBackups string

	// Status card
	ServerTitle    string
	ServerSubtitle string
	StatusOnline   string
	StatusBooting  string
	StatusStopping string
	StatusOffline  string
	LabelIP        string
	LabelHost      string
	LabelPort      string
	LabelPassword  string
	// CopyAddress titles the clipboard buttons beside the address values, and
	// CopyDone is what one says after it worked. They exist because a shared
	// endpoint gives players a provider hostname and a five-digit port that nobody
	// can retype from memory. Both are here rather than in the script for the
	// reason the script's own header gives: no user-visible string lives in the JS.
	CopyAddress string
	CopyDone    string
	// LocationTitle is the tooltip on the location badge. Only the tooltip is
	// translated: the value beside it is the place name the provider published, and
	// half-translating "Prague, CZ" would read worse than leaving it as it came.
	LocationTitle string

	// The tooltips on the three version badges. The values beside them are version
	// strings and are not translated; what needs a language is which version each
	// one is, since "v1.0.0" next to "v42.20.3" says nothing on its own.
	//
	// VersionCtlTitle covers every pzctl badge — the status panel and both file
	// cards — because they are deliberately one number: the tag that built the
	// controller built the packer that wrote those archives.
	VersionCtlTitle    string
	VersionServerTitle string
	VersionGameTitle   string

	// The three status banners that replace the address grid when there is no
	// address to show.
	BannerBootingTitle  string
	BannerBootingDesc   string
	BannerStoppingTitle string
	BannerStoppingDesc  string
	BannerOfflineTitle  string
	BannerOfflineDesc   string

	// The player count, and the two things v1 could not say. v1 rendered a
	// hardcoded 0 as "0 игроков", which is a measurement it never took.
	Players        Plural
	PlayersUnknown string
	PlayersStale   string

	// Torrent banner
	TorrentTitle string
	TorrentDesc  string
	TorrentBtn   string

	// Package cards
	CardClientTitle       string
	CardClientBtn         string
	CardCommonTitle       string
	CardCommonBtn         string
	CardServerTitle       string
	CardServerBtnLocked   string
	CardServerBtnUnlocked string
	StatsMods             Plural
	StatsFiles            Plural
	StatsReady            string

	// Guide
	GuideTitle string

	// Unlock modal. There is one, for server.zip: the backups page renders its
	// own lock server-side, so the modal's backups variant that v1 carried has
	// no caller and is not in the catalog. Neither is v1's "network error while
	// verifying" — the attempt is a form post now, so a failed one is the
	// browser's own error page rather than a message this page has to own.
	ModalServerTitle        string
	ModalServerDesc         string
	ModalServerPlaceholder  string
	ModalBackupsPlaceholder string
	ModalCancel             string
	ModalUnlock             string
	ModalVerifying          string
	ModalErrEmpty           string
	ModalErrWrong           string

	// Backups page
	BackupsPageTitle string
	BackupsTitle     string
	BackupsSubtitle  string
	Archives         Plural
	ThName           string
	ThDate           string
	ThSize           string
	ThAction         string
	Download         string
	Downloaded       string
	NotDownloaded    string
	NoBackups        string
	PwdRequired      string
	PwdDesc          string
	WrongPwd         string
	UnlockBtn        string
	DiskWarning      string
	RestoreTarget    string

	// Upload card. v1's was a multipart form POSTing to /upload; v2 keeps the
	// card because it is how an operator puts a downloaded archive back — the
	// system has no persistent storage, so restoring means uploading first.
	UploadTitle  string
	UploadDesc   string
	UploadBtn    string
	UploadBusy   string
	UploadDone   string
	UploadFailed string
}
