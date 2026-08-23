package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// prague is the configured timezone in every test here, and it is deliberately
// not the machine's: the whole point of identity.timezone is that a backup taken
// at 01:36 Prague reads as 01:36 whatever the container thinks the time is.
func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("loading Europe/Prague: %v", err)
	}
	return loc
}

func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Loc:               prague(t),
		Default:           RU,
		Locales:           Langs,
		Host:              "pz.example.com",
		DiskWarnPercent:   85,
		PlayersStaleAfter: 2 * time.Minute,
		PollInterval:      10 * time.Second,
	}
}

func onlineController() *state.Controller {
	return &state.Controller{
		Status:   state.StatusOnline,
		Endpoint: state.Endpoint{IP: "203.0.113.7", GamePort: 16261},
		Price:    state.Price{USDPerHour: 0.0213},
	}
}

// The four appearances, from the eight statuses the FSM has. The two mappings
// worth pinning are the last: a backup does not take the server off the page, and
// an online status without an endpoint is still booting rather than offline.
func TestStageOf(t *testing.T) {
	ready := state.Endpoint{IP: "203.0.113.7", GamePort: 16261}
	cases := []struct {
		status state.Status
		ep     state.Endpoint
		want   Stage
	}{
		{state.StatusOnline, ready, StageOnline},
		{state.StatusBackingUp, ready, StageOnline},
		{state.StatusDeploying, state.Endpoint{}, StageBooting},
		{state.StatusBooting, state.Endpoint{}, StageBooting},
		{state.StatusStopping, ready, StageStopping},
		{state.StatusClosing, state.Endpoint{}, StageStopping},
		{state.StatusOffline, state.Endpoint{}, StageOffline},
		{state.StatusFailed, state.Endpoint{}, StageOffline},

		// An endpoint with no port is not an address anyone can connect to.
		{state.StatusOnline, state.Endpoint{IP: "203.0.113.7"}, StageBooting},
		{state.StatusBackingUp, state.Endpoint{}, StageBooting},
	}
	for _, c := range cases {
		if got := stageOf(c.status, c.ep); got != c.want {
			t.Errorf("stageOf(%s, ready=%v) = %s, want %s", c.status, c.ep.Ready(), got, c.want)
		}
	}
}

// Bug 1's visible half. v1 printed a hardcoded zero as "0 игроков", so an empty
// server and a server nobody had asked looked identical.
func TestPlayerCountHasThreeStates(t *testing.T) {
	o := testOptions(t)
	in := Inputs{Controller: onlineController()}

	t.Run("unknown", func(t *testing.T) {
		in.Agent = &state.Agent{PlayersCount: state.PlayersUnknown}
		p := BuildPage(o, in, RU)
		if p.Players.Known {
			t.Fatal("an unmeasured count reported itself as known")
		}
		if p.Players.Text != catalog[RU].PlayersUnknown {
			t.Fatalf("text = %q, want the unknown message", p.Players.Text)
		}
		if strings.Contains(p.Players.Text, "0") {
			t.Fatalf("text = %q: an unknown count must not read as a number", p.Players.Text)
		}
	})

	t.Run("measured", func(t *testing.T) {
		in.Agent = &state.Agent{PlayersCount: 3, PlayersAt: state.Now(o.Loc)}
		p := BuildPage(o, in, RU)
		if !p.Players.Known || p.Players.Count != 3 {
			t.Fatalf("Known=%v Count=%d, want true/3", p.Players.Known, p.Players.Count)
		}
		if p.Players.Text != "3 игрока" {
			t.Fatalf("text = %q, want %q", p.Players.Text, "3 игрока")
		}
		if p.Players.Stale {
			t.Fatal("a fresh reading was marked stale")
		}
	})

	t.Run("measured zero is a measurement", func(t *testing.T) {
		in.Agent = &state.Agent{PlayersCount: 0, PlayersAt: state.Now(o.Loc)}
		p := BuildPage(o, in, RU)
		if !p.Players.Known || p.Players.Text != "0 игроков" {
			t.Fatalf("Known=%v text=%q, want a measured zero", p.Players.Known, p.Players.Text)
		}
	})

	t.Run("stale", func(t *testing.T) {
		in.Agent = &state.Agent{PlayersCount: 5, PlayersAt: state.At(time.Now().Add(-30 * time.Minute))}
		p := BuildPage(o, in, RU)
		if !p.Players.Known {
			t.Fatal("a stale reading is still a reading")
		}
		if !p.Players.Stale || p.Players.StaleText == "" {
			t.Fatalf("Stale=%v StaleText=%q, want a marked reading", p.Players.Stale, p.Players.StaleText)
		}
	})

	t.Run("counted but never stamped", func(t *testing.T) {
		// normalize.go demotes this, but the page must not depend on having been
		// handed a normalised document: an unstamped count is not a measurement.
		in.Agent = &state.Agent{PlayersCount: 7}
		if p := BuildPage(o, in, RU); p.Players.Known {
			t.Fatalf("an unstamped count was reported as measured: %q", p.Players.Text)
		}
	})

	t.Run("no agent document", func(t *testing.T) {
		in.Agent = nil
		if p := BuildPage(o, in, RU); p.Players.Known {
			t.Fatal("a missing agent document produced a count")
		}
	})
}

// The badge is only shown where a count answers a question anyone is asking, which
// is v1's rule and worth keeping: on an offline page "no data" is noise, and on a
// booting page it is noise that reads as a fault.
func TestPlayersBadgeOnlyAppearsOnline(t *testing.T) {
	o := testOptions(t)
	agent := &state.Agent{PlayersCount: 3, PlayersAt: state.Now(o.Loc)}

	cases := map[Stage]*state.Controller{
		StageOnline:   onlineController(),
		StageBooting:  {Status: state.StatusBooting},
		StageStopping: {Status: state.StatusStopping},
		StageOffline:  {Status: state.StatusOffline},
	}
	for stage, ctl := range cases {
		in := Inputs{Controller: ctl, Agent: agent}
		p := BuildPage(o, in, RU)
		want := stage == StageOnline
		if p.ShowPlayers != want {
			t.Errorf("%s: ShowPlayers = %v, want %v", stage, p.ShowPlayers, want)
		}
		// The poll must agree, or a page whose stage changed under it would keep
		// the badge the server rendered.
		if s := BuildStatus(o, in, RU); s.ShowPlayers != p.ShowPlayers {
			t.Errorf("%s: the poll says %v and the page says %v", stage, s.ShowPlayers, p.ShowPlayers)
		}
		// Hidden or not, the text is still rendered: the poll reveals the badge by
		// clearing an attribute, and an empty span would flash.
		if p.Players.Text == "" {
			t.Errorf("%s: no player text was rendered", stage)
		}
	}
}

// An online server whose count could not be measured has nothing to put in the
// badge but a denial, so there is no badge. This reverses v1's rule deliberately:
// an empty pill reading "players: no data" is worse than no pill, and it is what
// the operator saw through every startup. The text is still rendered underneath,
// because the poll reveals the badge by clearing an attribute and the span must
// not be empty when it does.
func TestPlayersBadgeNeedsAMeasurementNotJustOnline(t *testing.T) {
	o := testOptions(t)
	ctl := onlineController()

	for name, agent := range map[string]*state.Agent{
		"no agent at all": nil,
		"unknown count":   {PlayersCount: state.PlayersUnknown, PlayersAt: state.Now(o.Loc)},
		"count, no stamp": {PlayersCount: 4},
		"zero is a count": {PlayersCount: 0, PlayersAt: state.Now(o.Loc)},
	} {
		in := Inputs{Controller: ctl, Agent: agent}
		p := BuildPage(o, in, RU)
		want := name == "zero is a count"
		if p.ShowPlayers != want {
			t.Errorf("%s: ShowPlayers = %v, want %v", name, p.ShowPlayers, want)
		}
		if s := BuildStatus(o, in, RU); s.ShowPlayers != p.ShowPlayers {
			t.Errorf("%s: the poll says %v and the page says %v", name, s.ShowPlayers, p.ShowPlayers)
		}
		if p.Players.Text == "" {
			t.Errorf("%s: no player text was rendered", name)
		}
	}
}

// The location is the provider's, recorded on the lease, and is never invented. No
// lease means no badge — an offline page must not keep claiming the geography of a
// lease that has been closed.
func TestLocationComesFromTheLeaseOrNotAtAll(t *testing.T) {
	o := testOptions(t)

	ctl := onlineController()
	ctl.Lease = &state.Lease{DSeq: "1", Location: "Prague, CZ"}
	p := BuildPage(o, Inputs{Controller: ctl}, RU)
	if p.Location != "Prague, CZ" || !p.ShowLocation {
		t.Errorf("Location = %q / %v, want the lease's and shown", p.Location, p.ShowLocation)
	}

	// A provider that publishes no geography: Provider.Where returns "", which must
	// reach the page as no badge rather than as an empty pill.
	quiet := onlineController()
	quiet.Lease = &state.Lease{DSeq: "1"}
	if p := BuildPage(o, Inputs{Controller: quiet}, RU); p.ShowLocation {
		t.Error("a lease with no location still showed the badge")
	}

	if p := BuildPage(o, Inputs{Controller: &state.Controller{Status: state.StatusOffline}}, RU); p.ShowLocation {
		t.Error("an offline page with no lease showed a location")
	}
}

// The address grid appears exactly when there is somewhere to connect to, and the
// banner replaces it otherwise. Neither is ever both.
func TestAddressAndBanner(t *testing.T) {
	o := testOptions(t)
	o.JoinPassword = "hunter2"

	online := BuildPage(o, Inputs{Controller: onlineController()}, RU)
	if !online.ShowAddress {
		t.Fatal("an online server with an endpoint showed no address")
	}
	if online.Host != "pz.example.com" {
		t.Fatalf("Host = %q, want the DNS name", online.Host)
	}
	if online.JoinPassword != "hunter2" {
		t.Fatalf("JoinPassword = %q, want it shown alongside the address", online.JoinPassword)
	}

	off := BuildPage(o, Inputs{Controller: &state.Controller{Status: state.StatusOffline}}, RU)
	if off.ShowAddress {
		t.Fatal("an offline server showed an address")
	}
	if off.Banner.Class != "offline-banner" {
		t.Fatalf("Banner.Class = %q, want offline-banner", off.Banner.Class)
	}
	// There is nothing to connect to, so there is no reason for the join password
	// to be on the page.
	if off.JoinPassword != "" {
		t.Fatalf("JoinPassword = %q on an offline page", off.JoinPassword)
	}
}

func TestPriceIsHiddenWhenThereIsNoLease(t *testing.T) {
	o := testOptions(t)

	cheap := BuildPage(o, Inputs{Controller: onlineController()}, RU)
	if !cheap.ShowPrice || cheap.Price != "$0.021/hr" {
		t.Fatalf("Price = %q shown=%v, want $0.021/hr", cheap.Price, cheap.ShowPrice)
	}

	dear := onlineController()
	dear.Price.USDPerHour = 0.5
	if got := BuildPage(o, Inputs{Controller: dear}, RU); got.Price != "$0.50/hr" {
		t.Fatalf("Price = %q, want two decimals above a dime", got.Price)
	}

	// Offline: the number would be a quote for a lease that does not exist.
	gone := &state.Controller{Status: state.StatusOffline, Price: state.Price{USDPerHour: 0.02}}
	if got := BuildPage(o, Inputs{Controller: gone}, RU); got.ShowPrice {
		t.Fatalf("Price %q shown for an offline deployment", got.Price)
	}
}

// The server card is the only one that changes shape, and the shape carries v1's
// class names so the ported stylesheet needs no edits.
func TestServerCardLockAndUnlock(t *testing.T) {
	o := testOptions(t)
	in := Inputs{Controller: onlineController()}

	locked := serverCard(t, BuildPage(o, in, RU))
	if !locked.Locked {
		t.Fatal("the server card was not locked")
	}
	if locked.Href != "" {
		t.Fatalf("Href = %q on a locked card: v1 put the token in this attribute", locked.Href)
	}
	if locked.BtnClass != "card-btn btn-locked" || locked.Icon != "🔒" {
		t.Fatalf("locked presentation = %q/%q", locked.BtnClass, locked.Icon)
	}

	in.Unlocked.ServerFiles = true
	open := serverCard(t, BuildPage(o, in, RU))
	if open.Locked {
		t.Fatal("an unlocked card still reported itself locked")
	}
	if open.Href != "/server.zip" {
		t.Fatalf("Href = %q, want /server.zip", open.Href)
	}
	if open.Icon != "🔓" || open.BtnIcon != "⬇️" || open.BtnClass != "card-btn btn-unlocked" {
		t.Fatalf("unlocked presentation = %q/%q/%q", open.Icon, open.BtnIcon, open.BtnClass)
	}
	if !strings.Contains(open.CardClass, "unlocked") {
		t.Fatalf("CardClass = %q, want the unlocked modifier", open.CardClass)
	}
}

func serverCard(t *testing.T, p Page) Card {
	t.Helper()
	for _, c := range p.Cards {
		if c.Kind == "server" {
			return c
		}
	}
	t.Fatal("no server card on the page")
	return Card{}
}

// A count of zero is omitted rather than printed, for the same reason the player
// count distinguishes unknown from none.
func TestPackageStats(t *testing.T) {
	cases := []struct {
		lang  Lang
		stats PackageStats
		want  string
	}{
		{RU, PackageStats{Mods: 1, Files: 2, Size: 3 * 1024 * 1024}, "1 мод • 2 файла • 3.0 MB"},
		{RU, PackageStats{Mods: 12, Files: 340, Size: 134 * 1024 * 1024}, "12 модов • 340 файлов • 134.0 MB"},
		{RU, PackageStats{Files: 4, Size: 2048}, "4 файла • 2.0 KB"},
		{RU, PackageStats{}, "Готов"},
		{EN, PackageStats{Mods: 1, Files: 1, Size: 0}, "1 mod • 1 file • Ready"},
		{EN, PackageStats{Mods: 21, Files: 21, Size: 1024 * 1024}, "21 mods • 21 files • 1.0 MB"},
	}
	for _, c := range cases {
		if got := packageStats(c.lang, catalog[c.lang], c.stats); got != c.want {
			t.Errorf("packageStats(%s, %+v) = %q, want %q", c.lang, c.stats, got, c.want)
		}
	}
}

// v1's two size formats, both kept: packages to one decimal, archives to two.
func TestSizeText(t *testing.T) {
	const mib = 1024 * 1024
	cases := []struct {
		n      int64
		digits int
		zero   string
		want   string
	}{
		{0, 1, "Готов", "Готов"},
		{512, 1, "Готов", "0.5 KB"},
		{1024, 2, "0 KB", "1.0 KB"},
		{mib, 1, "-", "1.0 MB"},
		{mib, 2, "-", "1.00 MB"},
		{3*mib + mib/2, 2, "-", "3.50 MB"},
	}
	for _, c := range cases {
		if got := sizeText(c.n, c.digits, c.zero); got != c.want {
			t.Errorf("sizeText(%d, %d) = %q, want %q", c.n, c.digits, got, c.want)
		}
	}
}

// Both locales render, and the fallback is the configured default rather than a
// page with holes in it.
func TestLocaleSelection(t *testing.T) {
	o := testOptions(t)
	in := Inputs{Controller: onlineController()}

	if got := BuildPage(o, in, EN); got.Badge.Text != "ONLINE" {
		t.Fatalf("EN badge = %q", got.Badge.Text)
	}
	if got := BuildPage(o, in, RU); got.Badge.Text != "ОНЛАЙН" {
		t.Fatalf("RU badge = %q", got.Badge.Text)
	}
	if got := BuildPage(o, in, Lang("de")); got.Lang != RU {
		t.Fatalf("unknown locale rendered as %q, want the configured default", got.Lang)
	}

	o.Locales = []Lang{EN}
	if got := BuildPage(o, in, RU); got.Lang != EN {
		t.Fatalf("a locale not in the configured set rendered as %q", got.Lang)
	}
}

// BuildPage delegates the header to BuildStatus so the page and the poll cannot
// disagree. If someone inlines it again, this fails.
func TestPageAndStatusAgree(t *testing.T) {
	o := testOptions(t)
	fresh := state.Now(o.Loc)
	inputs := []Inputs{
		{Controller: onlineController(), Agent: &state.Agent{PlayersCount: 2, PlayersAt: fresh}},
		{Controller: &state.Controller{Status: state.StatusBooting}},
		{Controller: &state.Controller{Status: state.StatusStopping, Price: state.Price{USDPerHour: 0.02}}},
		{Controller: &state.Controller{Status: state.StatusFailed}},
		{Controller: &state.Controller{Status: state.StatusBackingUp, Endpoint: state.Endpoint{IP: "1.2.3.4", GamePort: 16261}}},
		{},
	}
	for _, lang := range Langs {
		for _, in := range inputs {
			p, s := BuildPage(o, in, lang), BuildStatus(o, in, lang)
			if p.Stage != s.Stage || p.Badge != s.Badge || p.Players != s.Players {
				t.Errorf("%s: page header %v/%v/%v differs from the poll's %v/%v/%v",
					lang, p.Stage, p.Badge, p.Players, s.Stage, s.Badge, s.Players)
			}
			if p.Price != s.Price || p.ShowPrice != s.ShowPrice {
				t.Errorf("%s: page price %q/%v differs from the poll's %q/%v",
					lang, p.Price, p.ShowPrice, s.Price, s.ShowPrice)
			}
		}
	}
}

// A nil controller document is a controller that has not written one yet, which
// is a real state during the first boot. It renders as offline, not as a panic.
func TestNilDocumentsRender(t *testing.T) {
	p := BuildPage(testOptions(t), Inputs{}, RU)
	if p.Stage != StageOffline {
		t.Fatalf("Stage = %s with no documents, want offline", p.Stage)
	}
	if len(p.Cards) != 3 {
		t.Fatalf("%d cards, want 3", len(p.Cards))
	}
}
