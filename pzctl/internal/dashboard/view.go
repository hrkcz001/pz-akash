package dashboard

import (
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Options is the configured half of a render: values that come from config.yaml
// and do not change between requests.
//
// It is a local struct rather than internal/config.Dashboard because the page
// needs values from four config blocks (identity, dns, game, dashboard) and one
// secret, and assembling them is the caller's job. That also means the whole
// view layer is testable from a literal.
type Options struct {
	// Loc is identity.timezone. Every timestamp on both pages is formatted
	// through it, which is the entire reason backup times read as Prague no
	// matter where the container runs.
	Loc *time.Location

	Default Lang
	Locales []Lang

	// Host is dns.game_record resolved to a name players can type. Empty when
	// no DNS record is configured, in which case the page shows the raw IP the
	// way v1 did.
	Host string

	// JoinPassword is shown on the status card when non-empty. v1 had the
	// password as a literal in the HTML, so there was no way not to show it;
	// here an operator who does not want it on a public page leaves
	// dashboard.show_join_password false and this stays empty.
	JoinPassword string

	// TorrentURL is the clean-client torrent. Empty hides the banner, which v1
	// could not do either.
	TorrentURL string

	// DiskWarnPercent is the usage at which the backups page starts asking for
	// downloads. Zero disables the warning.
	DiskWarnPercent int

	// PlayersStaleAfter is how old a player count may be before the page says
	// so. It comes from the agent's poll interval times a small tolerance: a
	// count older than that means the agent stopped answering, not that nobody
	// joined. Zero never marks a count stale.
	PlayersStaleAfter time.Duration

	// PollInterval is how often the open page asks /api/status for a fresh
	// reading. Zero disables polling, and the page is then only as current as
	// the last load.
	PollInterval time.Duration
}

// lang returns the locale to render, given whatever the request asked for.
//
// The fallback is checked against the configured set too. A default that is not
// in dashboard.locales would otherwise render a page in a locale the switcher does
// not offer — visible only as a switcher whose current entry is not highlighted,
// which is exactly the kind of thing nobody reports.
func (o Options) lang(want Lang) Lang {
	if len(o.Locales) == 0 {
		if o.Default != "" {
			return o.Default
		}
		return RU
	}
	for _, l := range o.Locales {
		if l == want {
			return l
		}
	}
	for _, l := range o.Locales {
		if l == o.Default {
			return l
		}
	}
	return o.Locales[0]
}

// Inputs is the per-request half: what is currently true.
//
// The controller and agent documents are the FSM's in-memory snapshot, not a
// file. That is the last piece of bug 3 removed — v1's dashboard read
// server_info.json out of a git working tree that the sync loop was checking out
// underneath it, which is why the log filled with "Expecting value: line 1
// column 106".
type Inputs struct {
	Controller *state.Controller
	Agent      *state.Agent
	Backups    *state.Backups

	Packages Packages

	// GameVersion is the Project Zomboid build the torrent contains, badged on the
	// clean-client card. It is the game's version, not this program's: the badge
	// used to be handed main.version, which CI sets to a git sha, so a card
	// offering a game client was labelled "vsha-2fd34d2". Empty omits the badge.
	GameVersion string

	// Guide is the markdown of README.<lang>.md, already selected for the
	// locale being rendered. Empty omits the section.
	Guide string

	Unlocked Unlocked

	// UnlockFailed re-renders the page with the lock still up and a reason under
	// it, which is how a wrong password is reported: the attempt is a POST to
	// /api/unlock and the answer is a redirect back here, so nothing on the page
	// ever asks "is this the password" on the visitor's behalf.
	UnlockFailed bool
	// UnlockRealm is which download the failed attempt was for, so the re-rendered
	// modal is still aimed at it.
	UnlockRealm string

	// DiskUsedPercent is -1 when it could not be measured.
	DiskUsedPercent int
}

// Unlocked records which guarded downloads this request may follow.
//
// v1 answered this question with a token in every href, so the password sat in
// browser history, in any referrer, and in the access log. Here it is a cookie
// the unlock endpoint sets, and the page only needs to know whether to render a
// link or a lock.
type Unlocked struct {
	ServerFiles bool
	Backups     bool
}

// Packages is the stats block from packages_manifest.json, which the packer
// writes next to the archives.
type Packages struct {
	Client PackageStats `json:"client"`
	Common PackageStats `json:"common"`
	Server PackageStats `json:"server"`
}

type PackageStats struct {
	Mods  int   `json:"mods_count"`
	Files int   `json:"files_count"`
	Size  int64 `json:"size"`
}

// Stage is the coarse state the page renders. The FSM has eight statuses; the
// page has four appearances, and they are v1's, so the port diffs clean.
type Stage string

const (
	StageOnline   Stage = "online"
	StageBooting  Stage = "booting"
	StageStopping Stage = "stopping"
	StageOffline  Stage = "offline"
)

// stageOf collapses a status and an endpoint into an appearance.
//
// Two mappings are worth stating. A backup running while the world stays up is
// online, because players are still connected and the page's job is to tell them
// where to connect. And StatusOnline without a ready endpoint renders as
// booting: v1 fell through to offline there, which showed "ОФФЛАЙН" for a server
// that was moments from accepting connections.
func stageOf(s state.Status, ep state.Endpoint) Stage {
	switch s {
	case state.StatusOnline, state.StatusBackingUp:
		if ep.Ready() {
			return StageOnline
		}
		return StageBooting
	case state.StatusDeploying, state.StatusBooting:
		return StageBooting
	case state.StatusStopping, state.StatusClosing:
		return StageStopping
	default:
		// StatusOffline, and StatusFailed. A failure is not a fifth appearance:
		// what a player needs to know is that they cannot connect, and the
		// reason belongs in the operator's log, not on a public page.
		return StageOffline
	}
}

// Badge is the status pill: v1's class names, so the stylesheet ports unchanged.
//
// It is serialised to the polling page, which is why the fields carry tags: the
// browser swaps the class as well as the text, or a server that went offline
// would keep a green pill.
type Badge struct {
	Class string `json:"class"` // badge-online, badge-booting, badge-stopping, badge-offline
	Dot   string `json:"dot"`   // the status-dot modifier
	Text  string `json:"text"`
}

func (s Stage) badge(t text) Badge {
	b := Badge{Class: "badge-" + string(s), Dot: string(s)}
	switch s {
	case StageOnline:
		b.Text = t.StatusOnline
	case StageBooting:
		b.Text = t.StatusBooting
	case StageStopping:
		b.Text = t.StatusStopping
	default:
		b.Text = t.StatusOffline
	}
	return b
}

// Banner is the block that replaces the address grid when there is no address.
type Banner struct {
	Class string
	Icon  string
	Title string
	Desc  string
}

func (s Stage) banner(t text) Banner {
	switch s {
	case StageBooting:
		return Banner{"booting-banner", "🚀", t.BannerBootingTitle, t.BannerBootingDesc}
	case StageStopping:
		return Banner{"stopping-banner", "🛑", t.BannerStoppingTitle, t.BannerStoppingDesc}
	default:
		return Banner{"offline-banner", "⏸️", t.BannerOfflineTitle, t.BannerOfflineDesc}
	}
}

// PlayersView is the player count, and the two things v1 could not express.
//
// v1 read players_count out of server_info.json, which the bash system wrote as
// a literal 0 and never updated — bug 1. The count was therefore always "0
// игроков", which is indistinguishable from a measurement of an empty server.
// The agent now polls the game console, so there are three cases, and the page
// says which one it is.
type PlayersView struct {
	Known bool   `json:"known"`
	Count int    `json:"count"`
	Text  string `json:"text"`

	// Stale means the count is a real measurement that has since aged past the
	// poll interval — the agent stopped answering, so the number on the page is
	// the last one it managed to take.
	Stale     bool   `json:"stale"`
	StaleText string `json:"stale_text"`
}

// Chrome is the part of a render both pages share: the document title, the nav
// bar, and the locale switcher. Both view structs embed it, so one template
// block serves both.
type Chrome struct {
	Lang     Lang
	T        text
	Switcher []LangLink

	// Title is the <title>. It differs between the pages, which is why it is
	// here rather than read off the catalog in the template.
	Title string

	// Active is the nav entry to mark: "packages" or "backups".
	Active string
}

// LangLink is one entry in the locale switcher.
//
// Href is query-only so it resolves against whatever path is being rendered:
// the same switcher works on the hub and on the backups page.
type LangLink struct {
	Lang    Lang
	Label   string
	Href    string
	Current bool
}

func (o Options) switcher(cur Lang) []LangLink {
	locales := o.Locales
	if len(locales) == 0 {
		locales = Langs
	}
	out := make([]LangLink, 0, len(locales))
	for _, l := range locales {
		out = append(out, LangLink{
			Lang:    l,
			Label:   strings.ToUpper(string(l)),
			Href:    "?lang=" + string(l),
			Current: l == cur,
		})
	}
	return out
}
