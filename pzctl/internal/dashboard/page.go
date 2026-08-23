package dashboard

import (
	"fmt"
	"html/template"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Page is the hub. Every field is finished text or a decided boolean: the
// template branches on state, it does not compute it. That split is deliberate —
// the logic that used to live in an f-string and in 400 lines of browser
// JavaScript is here, where a test can call it.
type Page struct {
	Chrome

	// GameVersion badges the torrent card with the Project Zomboid build it
	// contains. See Inputs.GameVersion for why it is not this program's version.
	GameVersion string
	// Version and ServerVersion badge the status panel: what is running the server,
	// and what build of the game it is running. See Options for both.
	Version       string
	ServerVersion string
	Stage         Stage
	Badge         Badge

	// PollMS is the status poll period in milliseconds, 0 when polling is off.
	// It reaches the script as a data attribute on <body> rather than as
	// generated JavaScript, so no value is ever interpolated into a script.
	PollMS int64

	// ShowAddress selects the address grid over the banner. It is exactly
	// "there is somewhere to connect to".
	ShowAddress bool
	Banner      Banner

	// Host is a name the player types, and the card showing it is labelled
	// "server address". Two different things end up here: the configured
	// dns.game_record when there is a zone, and otherwise the provider's own
	// hostname on a shared endpoint. Both are names, which is the only property
	// the page cares about. IP is set only for a dedicated IP lease, and the
	// template shows one card or the other.
	Host string
	IP   string
	Port int
	// JoinPassword is empty unless the operator opted into showing it.
	JoinPassword string

	Price     string
	ShowPrice bool

	Players PlayersView
	// ShowPlayers hides the badge outside StageOnline, as v1 did. See Status.
	ShowPlayers bool

	// Location is where the provider says the lease is — "Prague, CZ" or similar,
	// unlocalised, because it is a place name the provider chose and translating
	// half of it would be worse than leaving it alone.
	//
	// Page-only, and deliberately not in the polled Status: the location of a lease
	// cannot change while a lease exists, and getting a new lease moves the stage,
	// which makes the poll reload the whole page. So there is nothing for the poll
	// to keep in sync here — which is the one exception to the rule Status
	// documents, stated so the next reader does not have to guess whether it was an
	// oversight.
	Location     string
	ShowLocation bool

	Torrent *Torrent
	Cards   []Card

	// Guide is the rendered README. Empty omits the section rather than
	// printing a placeholder sentence in a language the page may not be in.
	Guide template.HTML

	Unlocked Unlocked

	// The unlock modal's server-rendered state. ModalOpen is set after a failed
	// attempt so the page comes back with the prompt still up and the reason
	// under it; ModalRealm keeps the form pointed at whichever download the
	// visitor was after.
	ModalOpen   bool
	ModalRealm  string
	UnlockError string
}

// Torrent is the clean-client banner.
type Torrent struct {
	URL   string
	Title string
	Desc  string
	Btn   string
}

// Card is one download tile.
//
// The class names are computed here rather than branched on in the template,
// because they are v1's and there are four of them per card. Keeping them in Go
// is what lets the ported stylesheet stay untouched and still be checked.
type Card struct {
	Kind string // client, common, server

	CardClass string // action-card card-client, plus unlocked on the server card
	IconClass string // client-icon-box
	Icon      string // 🎮 📦 🔒 🔓
	Title     string
	Stats     string

	// Version badges the tile with the pzctl build that packed the archive. Empty on
	// the server card, which is the operator's download and carries a lock instead.
	Version string

	BtnClass string // card-btn btn-client, btn-locked, btn-unlocked
	BtnIcon  string // the server card's 🔒/⬇️; empty where the SVG is used
	Btn      string

	// Href is empty for a locked card: the template renders a button that opens
	// the unlock modal instead of a link, so there is no URL carrying a token
	// the way v1's had.
	Href   string
	Locked bool
}

// newCard fills in the presentation for one package tile.
func newCard(kind, icon, title, stats, btn, href string) Card {
	return Card{
		Kind:      kind,
		CardClass: "action-card card-" + kind,
		IconClass: kind + "-icon-box",
		Icon:      icon,
		Title:     title,
		Stats:     stats,
		BtnClass:  "card-btn btn-" + kind,
		Btn:       btn,
		Href:      href,
	}
}

// BuildPage assembles the hub for one locale.
func BuildPage(o Options, in Inputs, want Lang) Page {
	lang := o.lang(want)
	t := catalog[lang]

	ctl := in.Controller
	if ctl == nil {
		ctl = &state.Controller{}
	}

	stage := stageOf(ctl.Status, ctl.Endpoint)
	st := BuildStatus(o, in, lang)
	// A configured DNS name wins; without one, a shared-endpoint lease still has a
	// name to show, and it is the provider's. Falling back here rather than in the
	// template keeps the "one card or the other" decision in one place — and stops
	// a provider hostname from being printed under the label "server IP".
	host := o.Host
	if host == "" {
		host = ctl.Endpoint.Host
	}
	// The lease is the only source: a location is a property of the provider we
	// took a bid from, so no lease means nothing true to say and the badge is
	// omitted. It appears as soon as the lease exists rather than at StageOnline,
	// because "booting, in Prague" is the more useful of the two sentences.
	location := ""
	if ctl.Lease != nil {
		location = ctl.Lease.Location
	}
	p := Page{
		Chrome: Chrome{
			Lang:     lang,
			T:        t,
			Switcher: o.switcher(lang),
			Title:    t.PageTitle,
			Active:   "connect",
		},
		GameVersion:   in.GameVersion,
		Version:       o.Version,
		ServerVersion: o.ServerVersion,
		Stage:         st.Stage,
		Badge:         st.Badge,
		PollMS:        o.PollInterval.Milliseconds(),
		ShowAddress:   stage == StageOnline,
		Banner:        stage.banner(t),
		Host:          host,
		IP:            ctl.Endpoint.IP,
		Port:          ctl.Endpoint.GamePort,
		Players:       st.Players,
		ShowPlayers:   st.ShowPlayers,
		Location:      location,
		ShowLocation:  location != "",
		Price:         st.Price,
		ShowPrice:     st.ShowPrice,
		Guide:         RenderMarkdown(in.Guide),
		Unlocked:      in.Unlocked,
	}

	if p.ShowAddress {
		p.JoinPassword = o.JoinPassword
	}

	// A failed unlock comes back as a rendered page with the modal already open,
	// rather than as a fetch that reports the verdict. v1's /api/verify answered
	// "is this the password" to anyone, as often as they asked.
	if in.UnlockFailed {
		p.ModalOpen, p.UnlockError = true, t.ModalErrWrong
		p.ModalRealm = in.UnlockRealm
	}

	if o.TorrentURL != "" {
		p.Torrent = &Torrent{URL: o.TorrentURL, Title: t.TorrentTitle, Desc: t.TorrentDesc, Btn: t.TorrentBtn}
	}

	p.Cards = []Card{
		newCard("client", "🎮", t.CardClientTitle, packageStats(lang, t, in.Packages.Client), t.CardClientBtn, "/client.zip"),
		newCard("common", "📦", t.CardCommonTitle, packageStats(lang, t, in.Packages.Common), t.CardCommonBtn, "/common.zip"),
	}
	// The two public archives carry the build that packed them, which is this build:
	// the image that serves them is the image that made them, so there is one number
	// and the page does not have to explain a mismatch it cannot have.
	for i := range p.Cards {
		p.Cards[i].Version = o.Version
	}

	// The server card is the one that changes shape. Unlocked it is a download
	// link like the other two; locked it is a button that opens the modal, and
	// it carries no URL at all — v1 put the token in the href, so the password
	// was in the DOM of a public page as soon as anyone had typed it once.
	server := newCard("server", "🔒", t.CardServerTitle, packageStats(lang, t, in.Packages.Server), t.CardServerBtnLocked, "")
	if in.Unlocked.ServerFiles {
		server.Icon, server.BtnIcon = "🔓", "⬇️"
		server.Btn, server.Href = t.CardServerBtnUnlocked, "/server.zip"
		server.CardClass += " unlocked"
		server.BtnClass = "card-btn btn-unlocked"
	} else {
		server.BtnIcon, server.Locked = "🔒", true
		server.BtnClass = "card-btn btn-locked"
	}
	p.Cards = append(p.Cards, server)

	return p
}

// buildPlayers renders the count, or says why there is not one.
func buildPlayers(o Options, a *state.Agent, lang Lang, t text) PlayersView {
	// Three cases, and v1 could express one. An unstamped count is not a
	// measurement: normalize.go demotes those to unknown, but the page must not
	// depend on having been handed a normalised document.
	if a == nil || !a.PlayersKnown() || a.PlayersAt.Zero() {
		return PlayersView{Text: t.PlayersUnknown}
	}
	v := PlayersView{
		Known: true,
		Count: a.PlayersCount,
		Text:  fmt.Sprintf("%d %s", a.PlayersCount, lang.pluralize(a.PlayersCount, t.Players)),
	}
	if o.PlayersStaleAfter > 0 && a.PlayersAt.Age() > o.PlayersStaleAfter {
		v.Stale, v.StaleText = true, t.PlayersStale
	}
	return v
}

// packageStats is v1's "12 модов • 340 файлов • 128.4 MB" subtitle.
//
// A count of zero is omitted rather than printed, so a package with no mods says
// "340 файлов • 128.4 MB" instead of claiming a measured zero — the same reason
// the player count distinguishes unknown from none. An empty package falls back
// to the "Готов" label, which is what v1 printed when the archive had no size.
func packageStats(lang Lang, t text, s PackageStats) string {
	var parts []string
	if s.Mods > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", s.Mods, lang.pluralize(s.Mods, t.StatsMods)))
	}
	if s.Files > 0 {
		parts = append(parts, fmt.Sprintf("%d %s", s.Files, lang.pluralize(s.Files, t.StatsFiles)))
	}
	parts = append(parts, sizeText(s.Size, 1, t.StatsReady))
	return strings.Join(parts, " • ")
}

// sizeText formats a byte count the way v1 did, down to the number of decimals:
// megabytes above a mebibyte, kilobytes below it, and a caller-supplied word
// when there is nothing there at all.
func sizeText(n int64, mbDigits int, zero string) string {
	const mib = 1024 * 1024
	switch {
	case n >= mib:
		return fmt.Sprintf("%.*f MB", mbDigits, float64(n)/mib)
	case n > 0:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return zero
	}
}

// priceText matches v1's two-format rule: three decimals for the fractions of a
// cent an Akash lease actually costs, two for anything a human would call a
// price.
func priceText(usdPerHour float64) string {
	if usdPerHour < 0.1 {
		return fmt.Sprintf("$%.3f/hr", usdPerHour)
	}
	return fmt.Sprintf("$%.2f/hr", usdPerHour)
}
