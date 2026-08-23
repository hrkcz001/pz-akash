package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/dashboard"
	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// dash builds a dashData over a temp packages_dir, with no machine and no file
// server: every method under test here is a function of the snapshot, the store
// and two files, and inputs is where the machine read was factored out to.
func dash(t *testing.T, mutate func(*config.Config)) (*dashData, string) {
	t.Helper()
	dir := t.TempDir()

	cfg := &config.Config{}
	cfg.Identity.Timezone = "Europe/Prague"
	cfg.Controller.Storage.PackagesDir = dir
	cfg.Dashboard = config.Dashboard{
		DefaultLocale: "ru",
		Locales:       []string{"ru", "en"},
		GuideFile:     "README.{lang}.md",
		TorrentFile:   "game.torrent",
		GameVersion:   "42.20.3",
	}
	if mutate != nil {
		mutate(cfg)
	}
	return newDashData(nil, nil, cfg, func(string, ...any) {}), dir
}

// write puts a file in packages_dir with a modification time far enough apart that
// a one-second filesystem timestamp still distinguishes the versions.
func write(t *testing.T, dir, name, body string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
}

// A saves repo with one README must serve both locales. This is the state pz-saves
// is actually in, and the alternative — no guide at all on every page — is how the
// port would have shipped looking like a regression.
func TestGuideFallsBackToTheUnsuffixedFile(t *testing.T) {
	d, dir := dash(t, nil)
	write(t, dir, "README.md", "# общий", time.Hour)

	for _, lang := range []dashboard.Lang{"ru", "en"} {
		if got := d.guide(lang); got != "# общий" {
			t.Fatalf("guide(%q) = %q, want the unsuffixed README", lang, got)
		}
	}

	// And a translation added later wins for its own locale without displacing the
	// fallback for the other.
	write(t, dir, "README.en.md", "# english", time.Hour)
	if got := d.guide("en"); got != "# english" {
		t.Fatalf("guide(en) = %q, want the translation once it exists", got)
	}
	if got := d.guide("ru"); got != "# общий" {
		t.Fatalf("guide(ru) = %q, want the fallback still", got)
	}
}

// The cache has to invalidate in both directions. Presence is the obvious one;
// absence is the one that matters, because a removed translation that keeps being
// served is a page showing text no file on disk contains.
func TestGuideCacheFollowsTheFileBothWays(t *testing.T) {
	d, dir := dash(t, nil)
	write(t, dir, "README.en.md", "first", time.Hour)
	if got := d.guide("en"); got != "first" {
		t.Fatalf("guide(en) = %q, want the file", got)
	}

	write(t, dir, "README.en.md", "second", 0)
	if got := d.guide("en"); got != "second" {
		t.Fatalf("after a rewrite guide(en) = %q, want the new contents", got)
	}

	if err := os.Remove(filepath.Join(dir, "README.en.md")); err != nil {
		t.Fatal(err)
	}
	if got := d.guide("en"); got != "" {
		t.Fatalf("after removal guide(en) = %q, want nothing — there is no file and no fallback", got)
	}

	write(t, dir, "README.en.md", "third", 0)
	if got := d.guide("en"); got != "third" {
		t.Fatalf("after re-creation guide(en) = %q, want the new file", got)
	}
}

func TestGuideIsOffWhenUnconfigured(t *testing.T) {
	d, dir := dash(t, func(c *config.Config) { c.Dashboard.GuideFile = "" })
	write(t, dir, "README.md", "# should not appear", time.Hour)
	if got := d.guide("ru"); got != "" {
		t.Fatalf("guide with no guide_file = %q, want nothing", got)
	}
}

// A manifest that does not parse must not take the page down with it: the counts
// are decoration and the downloads work without them. It must say so in the log,
// though — this is the shape of bug 3, and the thing that made bug 3 expensive was
// a message that named no file.
func TestBrokenManifestDegradesToNoCountsAndSaysSo(t *testing.T) {
	var logs []string
	d, dir := dash(t, nil)
	d.logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	write(t, dir, "packages_manifest.json", `{"client": {"mods_count": 3,`, time.Hour)
	if got := d.packages(); got != (dashboard.Packages{}) {
		t.Fatalf("a truncated manifest parsed to %+v, want the zero value", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "packages_manifest.json") {
		t.Fatalf("the log does not name the file: %v", logs)
	}

	write(t, dir, "packages_manifest.json", `{"client": {"mods_count": 3, "files_count": 9, "size": 42}}`, 0)
	got := d.packages()
	if got.Client.Mods != 3 || got.Client.Files != 9 || got.Client.Size != 42 {
		t.Fatalf("packages = %+v, want the manifest's client block", got)
	}
}

func TestDiskUsedPercent(t *testing.T) {
	cases := []struct {
		name       string
		used, free int64
		ok         bool
		want       int
	}{
		// The denominator is archives plus free space, not the volume: what an
		// operator can act on by downloading is the room the archives have left.
		{"half", 5 << 30, 5 << 30, true, 50},
		{"empty", 0, 20 << 30, true, 0},
		{"full", 20 << 30, 0, true, 100},
		// -1 is "could not measure", which the page renders as no warning at all
		// rather than as a reassuring 0%.
		{"probe failed", 5 << 30, 5 << 30, false, -1},
		{"nothing to divide by", 0, 0, true, -1},
	}
	for _, c := range cases {
		if got := diskUsedPercent(c.used, c.free, c.ok); got != c.want {
			t.Errorf("%s: diskUsedPercent(%d, %d, %v) = %d, want %d",
				c.name, c.used, c.free, c.ok, got, c.want)
		}
	}
}

// With no file server attached — the window between construction and the listener
// starting — every realm reads as locked. The opposite default would render links
// the download handler then refuses.
func TestWithoutAFileServerNothingIsUnlocked(t *testing.T) {
	d, _ := dash(t, nil)
	if u := d.Unlocked(nil); u.ServerFiles || u.Backups {
		t.Fatalf("Unlocked with no file server = %+v, want both false", u)
	}
	if d.Unlock(nil, nil, "backups", "anything") {
		t.Fatal("an unlock succeeded with no file server to verify against")
	}
}

// The FSM's copy of both documents is what the page renders — never a file in a
// working tree the sync loop is running `git reset --hard` on, which is bug 3.
func TestInputsComeFromTheSnapshotNotTheDisk(t *testing.T) {
	d, _ := dash(t, nil)
	loc := prague(t)

	ctrl := state.NewController(loc)
	ctrl.Endpoint = state.Endpoint{IP: "1.2.3.4", GamePort: 16261}
	agent := state.NewAgent(loc)
	agent.PlayersCount = 7

	in := d.inputs(fsm.Snapshot{Controller: ctrl, Agent: agent}, "ru")
	if in.Controller != ctrl || in.Agent != agent {
		t.Fatal("inputs did not carry the snapshot's documents through")
	}
	// No store: the page still renders, with the warning suppressed rather than
	// reporting a comfortable zero.
	if in.DiskUsedPercent != -1 {
		t.Fatalf("DiskUsedPercent with no store = %d, want -1", in.DiskUsedPercent)
	}
	// The version on the page is the game's, from config. Never main.version: CI
	// sets that to a git sha, and it once labelled the clean-client card
	// "vsha-2fd34d2".
	if in.GameVersion != "42.20.3" {
		t.Fatalf("GameVersion = %q, want the configured game build", in.GameVersion)
	}
	if in.GameVersion == version {
		t.Fatal("GameVersion is this build's version; it must come from dashboard.game_version")
	}
}

// Without a store the machine's own list is still worth showing: the archives are
// on the agent's side of the wire, and a list an operator cannot click is better
// than a page that claims there are none.
func TestInputsFallBackToTheMachinesBackupList(t *testing.T) {
	d, _ := dash(t, nil)
	items := []state.Backup{{Name: "backup_20260819_013623.zip", Size: 10}}

	in := d.inputs(fsm.Snapshot{Backups: items}, "ru")
	if in.Backups == nil || len(in.Backups.Items) != 1 || in.Backups.Items[0].Name != items[0].Name {
		t.Fatalf("Backups = %+v, want the snapshot's list", in.Backups)
	}
}

func TestDashboardOptionsCarryTheConfiguredHalf(t *testing.T) {
	cfg := &config.Config{}
	cfg.Identity.Timezone = "Europe/Prague"
	cfg.Dashboard = config.Dashboard{
		DefaultLocale:     "ru",
		Locales:           []string{"ru", "en"},
		TorrentFile:       "game.torrent",
		PollInterval:      config.Duration(10 * time.Second),
		PlayersStaleAfter: config.Duration(5 * time.Minute),
	}
	cfg.Backups.DiskWarnPercent = 80
	cfg.DNS = config.DNS{Enabled: true, Domain: "example.com", GameRecord: "pz"}

	o := dashboardOptions(cfg, nil)
	if o.Loc == nil || o.Loc.String() != "Europe/Prague" {
		t.Fatalf("Loc = %v, want the configured timezone and never the host's", o.Loc)
	}
	if o.Default != "ru" || len(o.Locales) != 2 {
		t.Fatalf("locales = %v/%v", o.Default, o.Locales)
	}
	if o.Host != "pz.example.com" {
		t.Fatalf("Host = %q, want the game record", o.Host)
	}
	if o.TorrentURL == "" {
		t.Fatal("a configured torrent_file did not produce a link")
	}
	if o.DiskWarnPercent != 80 || o.PollInterval != 10*time.Second || o.PlayersStaleAfter != 5*time.Minute {
		t.Fatalf("options = %+v", o)
	}

	cfg.Dashboard.TorrentFile = ""
	if got := dashboardOptions(cfg, nil).TorrentURL; got != "" {
		t.Fatalf("TorrentURL with no torrent_file = %q, want nothing — the banner has to be hideable", got)
	}
}

// TestTheTwoStatusBadgesComeFromDifferentPlaces is the wiring half of the version
// badges. One is the binary's own tag and one is a config value, and the reason to
// pin it is the history: main.version used to reach the page as a game version, and
// "vsha-2fd34d2" is what a player saw where a Project Zomboid build should have been.
func TestTheTwoStatusBadgesComeFromDifferentPlaces(t *testing.T) {
	cfg := &config.Config{}
	cfg.Identity.Timezone = "Europe/Prague"
	cfg.Dashboard.ServerVersion = "42.20.3"

	o := dashboardOptions(cfg, nil)
	if o.Version != version {
		t.Errorf("Version = %q, want this build's own version %q", o.Version, version)
	}
	if o.ServerVersion != "42.20.3" {
		t.Errorf("ServerVersion = %q, want dashboard.server_version", o.ServerVersion)
	}
	if o.ServerVersion == o.Version {
		t.Error("the two badges carry the same value; one of them is reading the wrong source")
	}

	// An unset server_version omits its badge rather than borrowing the other's
	// number. A deployment that has not filled it in has nothing true to say about
	// which game build is running, and the page says nothing.
	cfg.Dashboard.ServerVersion = ""
	if got := dashboardOptions(cfg, nil); got.ServerVersion != "" {
		t.Errorf("ServerVersion with nothing configured = %q, want none", got.ServerVersion)
	} else if got.Version == "" {
		t.Error("the pzctl badge went away with it; they are independent")
	}
}

// Three conditions, all necessary. v1 wrote the password into the HTML with the
// same f-string as the address, so there was no way to publish one without the
// other.
func TestJoinPasswordNeedsAllThreeConditions(t *testing.T) {
	set := &secrets.Set{JoinPassword: "join-hunter2"}
	base := func() *config.Config {
		c := &config.Config{}
		c.Identity.Timezone = "Europe/Prague"
		c.Dashboard.ShowJoinPassword = true
		c.Game.PasswordProtected = true
		return c
	}

	if got := dashboardOptions(base(), set).JoinPassword; got != set.JoinPassword {
		t.Fatalf("JoinPassword = %q, want it shown when all three hold", got)
	}

	off := base()
	off.Dashboard.ShowJoinPassword = false
	if got := dashboardOptions(off, set).JoinPassword; got != "" {
		t.Fatalf("show_join_password: false still published %q", got)
	}

	open := base()
	open.Game.PasswordProtected = false
	if got := dashboardOptions(open, set).JoinPassword; got != "" {
		t.Fatalf("a server with no join password published %q", got)
	}

	if got := dashboardOptions(base(), nil).JoinPassword; got != "" {
		t.Fatalf("a controller with no secrets published %q", got)
	}
}
