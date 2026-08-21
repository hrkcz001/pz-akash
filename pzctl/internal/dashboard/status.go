package dashboard

import "github.com/hrkcz001/pz-akash/pzctl/internal/state"

// Status is what an open page polls for: the parts of the header that change
// while nobody navigates.
//
// It is JSON because the alternative is v1's, which was to send the whole page
// again and let 400 lines of browser JavaScript find the elements to patch. The
// strings here are still rendered by Go, for the locale of the polling request —
// the browser copies them into place, it does not choose them.
type Status struct {
	Stage   Stage       `json:"stage"`
	Badge   Badge       `json:"badge"`
	Players PlayersView `json:"players"`
	// ShowPlayers hides the badge outside the stages where a count means
	// anything. v1 hid it whenever the server was not online, and it was right to:
	// on an offline page the honest "no data" is noise, and on a booting one it is
	// noise that looks like a fault. Keeping the rule also sharpens the unknown
	// text, which now only ever appears where it is the actual news — an online
	// server whose count could not be measured, which is bug 1.
	ShowPlayers bool `json:"show_players"`

	Price     string `json:"price"`
	ShowPrice bool   `json:"show_price"`
}

// BuildStatus renders the polled header for one locale.
//
// BuildPage calls this rather than repeating it, so a change to how a stage or a
// player count is presented cannot apply to the page and miss the poll — which
// would show a stale badge until the next navigation and be very hard to see.
func BuildStatus(o Options, in Inputs, want Lang) Status {
	lang := o.lang(want)
	t := catalog[lang]

	ctl := in.Controller
	if ctl == nil {
		ctl = &state.Controller{}
	}

	stage := stageOf(ctl.Status, ctl.Endpoint)
	s := Status{
		Stage:   stage,
		Badge:   stage.badge(t),
		Players: buildPlayers(o, in.Agent, lang, t),
		// The same stage the address grid appears at, which is not a coincidence:
		// both answer questions that only exist once players can connect.
		ShowPlayers: stage == StageOnline,
	}

	// v1 showed the price whenever the deployment was costing anything, and hid
	// it when offline — where the number would be a quote for a lease that does
	// not exist.
	if stage != StageOffline && ctl.Price.USDPerHour > 0 {
		s.Price, s.ShowPrice = priceText(ctl.Price.USDPerHour), true
	}
	return s
}
