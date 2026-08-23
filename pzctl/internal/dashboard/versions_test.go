package dashboard

// The three version badges. They are one feature but three different facts, and the
// only interesting thing about them is that they must not be confused: "pzctl v1.0.0"
// beside "PZ v42.20.3" is informative, and either number printed under the other's
// label is a support conversation nobody can win.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// versionOptions is testOptions with both build numbers set and a torrent to badge,
// which is the shipped configuration: config.yaml names server_version and
// game_version, and Version is stamped into the binary at link time.
func versionOptions(t *testing.T) Options {
	t.Helper()
	o := testOptions(t)
	o.Version = "v1.0.0"
	o.ServerVersion = "42.20.3"
	o.TorrentURL = "/game.torrent"
	return o
}

// TestVersionBadgesKeepTheirSourcesApart is the whole point of having three fields
// where one would compile. Version comes from the binary, ServerVersion and
// GameVersion from config, and the two config values can legitimately differ — a
// server updated before the torrent was repacked is the normal state of an update
// day, and it is exactly when a player reads these numbers.
func TestVersionBadgesKeepTheirSourcesApart(t *testing.T) {
	o := versionOptions(t)
	in := Inputs{Controller: onlineController(), GameVersion: "42.19.1"}
	p := BuildPage(o, in, RU)

	if p.Version != "v1.0.0" {
		t.Errorf("Version = %q, want the build's own version", p.Version)
	}
	if p.ServerVersion != "42.20.3" {
		t.Errorf("ServerVersion = %q, want dashboard.server_version", p.ServerVersion)
	}
	if p.GameVersion != "42.19.1" {
		t.Errorf("GameVersion = %q, want the torrent's build, even when it lags the server's",
			p.GameVersion)
	}
}

// TestTheBuildBadgeIsTheSameNumberEverywhereItAppears pins the claim Options.Version
// makes: one tag builds the controller, the agent, and the image that packed the two
// public archives, so three numbers there would describe a system that cannot exist.
// The server card is the deliberate exception — its header carries a lock instead.
func TestTheBuildBadgeIsTheSameNumberEverywhereItAppears(t *testing.T) {
	o := versionOptions(t)
	p := BuildPage(o, Inputs{Controller: onlineController()}, RU)

	want := map[string]string{"client": "v1.0.0", "common": "v1.0.0", "server": ""}
	seen := map[string]bool{}
	for _, c := range p.Cards {
		w, ok := want[c.Kind]
		if !ok {
			t.Errorf("unexpected card kind %q", c.Kind)
			continue
		}
		seen[c.Kind] = true
		if c.Version != w {
			t.Errorf("the %s card's Version = %q, want %q", c.Kind, c.Version, w)
		}
	}
	for kind := range want {
		if !seen[kind] {
			t.Errorf("no %s card at all", kind)
		}
	}
	if p.Version != "v1.0.0" {
		t.Errorf("status panel Version = %q, want the same number the cards carry", p.Version)
	}
}

// TestVersionBadgesAreOmittedRatherThanEmpty covers the default. All four are
// {{with}} blocks, so an unset value must produce no badge — not an empty one, and in
// particular not a bare "v" or the word "dev" beside a game build. A development build
// has no tag, and this is the configuration it renders in.
func TestVersionBadgesAreOmittedRatherThanEmpty(t *testing.T) {
	o := testOptions(t)
	o.TorrentURL = "/game.torrent"
	p := BuildPage(o, Inputs{Controller: onlineController()}, RU)

	if p.Version != "" || p.ServerVersion != "" || p.GameVersion != "" {
		t.Fatalf("Version=%q ServerVersion=%q GameVersion=%q, want all three empty",
			p.Version, p.ServerVersion, p.GameVersion)
	}
	for _, c := range p.Cards {
		if c.Version != "" {
			t.Errorf("the %s card carries %q with no build version configured", c.Kind, c.Version)
		}
	}
}

// TestVersionBadgesReachThePageWithTheirOwnTooltips is the rendered half, and the
// reason it exists is the tooltips rather than the numbers: three badges reading
// "v1.0.0", "PZ v42.20.3" and "v42.20.3" are indistinguishable without them, and a
// title attribute is not something a view-model test can check.
func TestVersionBadgesReachThePageWithTheirOwnTooltips(t *testing.T) {
	o := versionOptions(t)
	in := Inputs{
		Controller:  onlineController(),
		GameVersion: "42.20.3",
		Packages: Packages{
			Client: PackageStats{Mods: 12, Files: 340, Size: 134 << 20},
			Common: PackageStats{Mods: 12, Files: 18, Size: 2 << 20},
			Server: PackageStats{Files: 26, Size: 96 << 10},
		},
	}
	h := newTestHandler(t, o, &galleryData{in: in})
	w := do(h, http.MethodGet, PathConnect+"?lang=en")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", PathConnect, w.Code, w.Body.String())
	}
	body := w.Body.String()

	en := catalog[EN]
	for _, want := range []string{
		// The status panel: this build, then the game build it is running.
		`<span>pzctl v1.0.0</span>`,
		`title="` + en.VersionCtlTitle + `"`,
		`<span>PZ v42.20.3</span>`,
		`title="` + en.VersionServerTitle + `"`,
		// The torrent card, whose number is the client's and whose tooltip says so.
		`title="` + en.VersionGameTitle + `">v42.20.3</span>`,
		// And the two public archives.
		`card-version" title="` + en.VersionCtlTitle + `">v1.0.0</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page does not contain %s", want)
		}
	}
	// Two archive badges, not three: the server card is locked and carries no build
	// number, so a third would mean the loop that stamps them ran after the append.
	if n := strings.Count(body, "card-version"); n != 2 {
		t.Errorf("%d card version badges, want 2 (client and common, not server)", n)
	}
}

// TestTheStatusPanelBadgesSurviveEveryStage is the one that would have caught a badge
// wired into the online branch alone. These are not readings — a version does not
// change while a page is open, and the poll never touches them — so an operator
// looking at an offline or failed server must still be able to see which build is
// refusing to start.
func TestTheStatusPanelBadgesSurviveEveryStage(t *testing.T) {
	o := versionOptions(t)
	for name, ctl := range map[string]*state.Controller{
		"online":  onlineController(),
		"booting": {Status: state.StatusBooting},
		"offline": {Status: state.StatusOffline},
		"failed":  {Status: state.StatusFailed, LastError: "no bids within 300s"},
	} {
		p := BuildPage(o, Inputs{Controller: ctl}, RU)
		if p.Version != "v1.0.0" || p.ServerVersion != "42.20.3" {
			t.Errorf("%s: Version=%q ServerVersion=%q, want both badges",
				name, p.Version, p.ServerVersion)
		}
	}
	// And with no documents at all, which is a controller that has just started and
	// not yet read the branch. It knows its own version before it knows anything else.
	p := BuildPage(o, Inputs{}, RU)
	if p.Version != "v1.0.0" || p.ServerVersion != "42.20.3" {
		t.Errorf("no state: Version=%q ServerVersion=%q, want both badges",
			p.Version, p.ServerVersion)
	}
}
