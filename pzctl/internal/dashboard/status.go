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
	// ShowPlayers hides the badge unless there is a measurement to show. Two
	// conditions, and each removes a different kind of noise: off StageOnline a
	// count means nothing and reads as a fault on a server that is merely booting,
	// and an online server whose count could not be measured has nothing to put in
	// the badge but a denial. An empty pill saying "no data" is worse than no pill —
	// the number appears when it exists.
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
	players := buildPlayers(o, in.Agent, lang, t)
	s := Status{
		Stage:   stage,
		Badge:   stage.badge(t),
		Players: players,
		// StageOnline is the same stage the address grid appears at, which is not a
		// coincidence: both answer questions that only exist once players can
		// connect. Known is the second half — see the field comment.
		ShowPlayers: stage == StageOnline && players.Known,
	}

	// v1 showed the price whenever the deployment was costing anything, and hid
	// it when offline — where the number would be a quote for a lease that does
	// not exist.
	if stage != StageOffline && ctl.Price.USDPerHour > 0 {
		s.Price, s.ShowPrice = priceText(ctl.Price.USDPerHour), true
	}
	return s
}
