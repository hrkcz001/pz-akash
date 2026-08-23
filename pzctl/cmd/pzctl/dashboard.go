package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/dashboard"
	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// dashData is the dashboard's view of the running controller.
//
// It lives here rather than in internal/dashboard because it is the seam between
// four things that package deliberately knows nothing about: the machine's
// snapshot, the file server's realms, the store's index, and two files on disk.
// Keeping it out means the view layer is testable from a struct literal, which is
// what let the whole of step 7's rendering be written before any of this existed.
type dashData struct {
	m     *fsm.Machine
	files *httpapi.Server
	store *httpapi.Store
	dir   string // controller.storage.packages_dir
	cfg   config.Dashboard
	logf  func(string, ...any)

	// The two files below are read from disk on demand and cached against their
	// modification time. Neither changes while the container runs — both are built
	// into the image — but a redeploy is not the only way they can change, and a
	// stat per request is cheaper than the alternative of reading a README on every
	// page load, which is what v1 did.
	mu     sync.Mutex
	pkg    fileCache[dashboard.Packages]
	guides map[string]*fileCache[string]
}

// fileCache is one file's parsed contents, valid while its mtime and size hold.
type fileCache[T any] struct {
	v       T
	modTime time.Time
	size    int64
	read    bool
}

func newDashData(m *fsm.Machine, store *httpapi.Store,
	cfg *config.Config, logf func(string, ...any)) *dashData {

	return &dashData{
		m:      m,
		store:  store,
		dir:    cfg.Controller.Storage.PackagesDir,
		cfg:    cfg.Dashboard,
		logf:   logf,
		guides: map[string]*fileCache[string]{},
	}
}

// use attaches the file server, which cannot be passed to the constructor: the
// file server takes this handler as its Extra, so one of the two has to be built
// first. The assignment happens before the listener goroutine starts, which is the
// happens-before edge that makes it safe without a lock — and until it happens
// every realm reads as locked, which is the right way round for a window that
// closes before the port is open.
func (d *dashData) use(files *httpapi.Server) { d.files = files }

// Snapshot assembles one render's worth of truth.
//
// Nothing here reads a file the FSM is writing, and nothing takes the machine's
// lock for longer than a struct copy. That is bug 3's root cause removed rather
// than papered over: v1 read server_info.json out of a working tree that the sync
// loop was running `git reset --hard` on, so a page load during a sync got half a
// document and the log filled with "Expecting value: line 1 column 106".
func (d *dashData) Snapshot(lang dashboard.Lang) dashboard.Inputs {
	return d.inputs(d.m.State(), lang)
}

// inputs is Snapshot with the machine read already done, which is the only part a
// test cannot supply: everything below is a pure function of the snapshot, the
// store and two files.
func (d *dashData) inputs(snap fsm.Snapshot, lang dashboard.Lang) dashboard.Inputs {
	in := dashboard.Inputs{
		Controller: snap.Controller,
		Agent:      snap.Agent,
		// The game's version, from config — not main.version, which CI sets to a git
		// sha and which labelled the clean-client card "vsha-2fd34d2".
		GameVersion: d.cfg.GameVersion,
		Packages:    d.packages(),
		Guide:       d.guide(lang),
	}
	if d.store != nil {
		in.Backups = d.store.Index()
		in.DiskUsedPercent = diskUsedPercent(d.store.Usage())
	} else {
		// No storage layer. -1 is "could not measure", which the page renders as no
		// warning at all rather than as a reassuring 0%.
		in.DiskUsedPercent = -1
		if len(snap.Backups) > 0 {
			// The machine still has whatever the agent reported, and a list of
			// archives an operator cannot download is still worth showing.
			in.Backups = &state.Backups{Items: snap.Backups}
		}
	}
	return in
}

// diskUsedPercent turns the store's two numbers into the percentage the warning is
// configured against.
//
// The denominator is the archives plus the free space, not the size of the volume:
// what the operator can act on is the room the archives have left to grow into, and
// the rest of the disk is the image, which downloading a backup will not free.
func diskUsedPercent(used, free int64, ok bool) int {
	if !ok || used+free <= 0 {
		return -1
	}
	return int(used * 100 / (used + free))
}

// Unlocked asks the file server, so the page cannot render a link to something the
// download handler would refuse — they are the same predicate, not two copies of
// one rule that drift.
func (d *dashData) Unlocked(r *http.Request) dashboard.Unlocked {
	if d.files == nil {
		return dashboard.Unlocked{}
	}
	return dashboard.Unlocked{
		ServerFiles: d.files.Unlocked(httpapi.RealmServerFiles, r),
		Backups:     d.files.Unlocked(httpapi.RealmBackups, r),
	}
}

// Unlock hands the password to the file server, which owns the comparison, the
// rate limit and the cookie.
func (d *dashData) Unlock(w http.ResponseWriter, r *http.Request, realm, password string) bool {
	if d.files == nil {
		return false
	}
	return d.files.Unlock(w, r, httpapi.Realm(realm), password)
}

// --- the two files ---

// packages reads packages_manifest.json, which the image build writes next to the
// archives. A missing or unreadable manifest renders the cards as "Ready" rather
// than as an error: the download still works, and the counts are decoration.
func (d *dashData) packages() dashboard.Packages {
	path := filepath.Join(d.dir, "packages_manifest.json")
	d.mu.Lock()
	defer d.mu.Unlock()

	fi := statOrNil(path)
	if d.pkg.matches(fi) {
		return d.pkg.v
	}
	var p dashboard.Packages
	if fi != nil {
		switch b, err := os.ReadFile(path); {
		case err != nil:
			d.logf("dashboard: reading %s: %v", path, err)
		default:
			if err := json.Unmarshal(b, &p); err != nil {
				d.logf("dashboard: %s is not valid JSON (%v) — package counts omitted", path, err)
				p = dashboard.Packages{}
			}
		}
	}
	d.pkg.store(p, fi)
	return p
}

// guide reads the markdown shown under the cards, for one locale.
//
// dashboard.guide_file may carry {lang}; the unsuffixed name is the fallback, so
// a saves repo holding a single README.md serves both locales, and a README.en.md
// added later is picked up with no code change.
func (d *dashData) guide(lang dashboard.Lang) string {
	if d.cfg.GuideFile == "" {
		return ""
	}
	name := strings.ReplaceAll(d.cfg.GuideFile, "{lang}", string(lang))
	if s := d.readGuide(name); s != "" {
		return s
	}
	if fallback := strings.ReplaceAll(d.cfg.GuideFile, ".{lang}", ""); fallback != name {
		return d.readGuide(fallback)
	}
	return ""
}

func (d *dashData) readGuide(name string) string {
	path := filepath.Join(d.dir, filepath.Base(name))
	d.mu.Lock()
	defer d.mu.Unlock()

	c, ok := d.guides[name]
	if !ok {
		c = &fileCache[string]{}
		d.guides[name] = c
	}
	fi := statOrNil(path)
	if c.matches(fi) {
		return c.v
	}
	var body string
	if fi != nil {
		b, err := os.ReadFile(path)
		if err != nil {
			d.logf("dashboard: reading %s: %v", path, err)
		}
		body = string(b)
	}
	c.store(body, fi)
	return body
}

func statOrNil(path string) fs.FileInfo {
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return fi
}

// matches reports whether the cached value still stands for fi, where a nil fi is
// a file that is not there.
//
// Absence is cached as well as presence, which is the case that matters for the
// guide: a translated README that is removed must stop being served immediately,
// and one that appears must start, so both directions have to invalidate.
func (c *fileCache[T]) matches(fi fs.FileInfo) bool {
	if !c.read {
		return false
	}
	if fi == nil {
		return c.size == -1
	}
	return c.size == fi.Size() && c.modTime.Equal(fi.ModTime())
}

// store records the value against the same FileInfo the read was decided by,
// never a fresh stat. Re-statting here would stamp a value read before a write
// with the modification time from after it, and the stale copy would then be
// served until the file changed a second time.
func (c *fileCache[T]) store(v T, fi fs.FileInfo) {
	c.v, c.read = v, true
	if fi == nil {
		c.size, c.modTime = -1, time.Time{}
		return
	}
	c.size, c.modTime = fi.Size(), fi.ModTime()
}

// dashboardOptions assembles the configured half of every render.
//
// Every one of these was a literal in v1's storage_server.py, including the join
// password, which was written into the HTML by the same f-string as the address —
// so there was no way to have an address on the page without the password beside
// it.
func dashboardOptions(cfg *config.Config, sec *secrets.Set) dashboard.Options {
	o := dashboard.Options{
		Loc:               cfg.Location(),
		Default:           dashboard.Lang(cfg.Dashboard.DefaultLocale),
		Host:              cfg.DNS.GameHost(),
		DiskWarnPercent:   cfg.Backups.DiskWarnPercent,
		PlayersStaleAfter: cfg.Dashboard.PlayersStaleAfter.D(),
		PollInterval:      cfg.Dashboard.PollInterval.D(),
		// This build's own version, and the only place on the page where main.version
		// belongs: it labels pzctl, which is what it is. The badge that used to carry
		// it labelled a game client, which is what made "vsha-2fd34d2" reach a player.
		Version:       version,
		ServerVersion: cfg.Dashboard.ServerVersion,
	}
	for _, l := range cfg.Dashboard.Locales {
		o.Locales = append(o.Locales, dashboard.Lang(l))
	}
	if cfg.Dashboard.TorrentFile != "" {
		o.TorrentURL = httpapi.PathTorrent
	}
	// Three conditions, all of them necessary: the operator asked for it, the game
	// actually has a join password, and this process has secrets to show. Any one
	// missing renders the address on its own.
	if cfg.Dashboard.ShowJoinPassword && cfg.Game.PasswordProtected && sec != nil {
		o.JoinPassword = sec.JoinPassword
	}
	return o
}
