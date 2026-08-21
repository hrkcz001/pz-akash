package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// TestWriteGallery renders every appearance to a file, for the visual diff against
// v1's pages. It is skipped unless PZ_GALLERY_DIR names a directory to write into:
//
//	PZ_GALLERY_DIR=../../../scratch/gallery go test ./internal/dashboard/ -run TestWriteGallery
//
// A gallery rather than golden files on purpose. Golden HTML would pin the markup
// and fail on every whitespace change, which for a port whose only requirement is
// feature parity would be noise; what has to be checked here is that a person
// looking at the two sets of pages sees the same thing, and that is a judgement a
// test cannot make.
func TestWriteGallery(t *testing.T) {
	dir := os.Getenv("PZ_GALLERY_DIR")
	if dir == "" {
		t.Skip("set PZ_GALLERY_DIR to write the gallery")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	o := fullOptions(t)
	o.TorrentURL = "/game.torrent"
	now := state.Now(o.Loc)

	// One case per appearance the operator or a player can actually meet. The
	// names become filenames, so they are also the index.
	cases := []struct {
		name string
		in   Inputs
		open Unlocked
	}{
		{
			name: "online",
			in: Inputs{
				Controller: &state.Controller{
					Status:   state.StatusOnline,
					Endpoint: state.Endpoint{IP: "203.0.113.7", GamePort: 16261, UDPPort: 16262},
					Price:    state.Price{USDPerHour: 0.0213, USDPerDay: 0.51},
					Since:    now,
				},
				Agent: &state.Agent{PlayersCount: 3, PlayersAt: now, LivenessAt: now},
			},
		},
		{
			// Bug 1's visible half: a count nobody managed to take must not read as
			// an empty server.
			name: "online-players-unknown",
			in: Inputs{
				Controller: &state.Controller{
					Status:   state.StatusOnline,
					Endpoint: state.Endpoint{IP: "203.0.113.7", GamePort: 16261},
					Since:    now,
				},
				Agent: &state.Agent{PlayersCount: state.PlayersUnknown, LivenessAt: now},
			},
		},
		{
			// A real measurement that has since aged out: the agent stopped
			// answering, so the number is the last one it took.
			name: "online-players-stale",
			in: Inputs{
				Controller: &state.Controller{
					Status:   state.StatusOnline,
					Endpoint: state.Endpoint{IP: "203.0.113.7", GamePort: 16261},
					Since:    now,
				},
				Agent: &state.Agent{
					PlayersCount: 5,
					PlayersAt:    state.Stamp{Time: now.Time.Add(-30 * time.Minute)},
					LivenessAt:   now,
				},
			},
		},
		{
			name: "booting",
			in: Inputs{
				Controller: &state.Controller{Status: state.StatusBooting, Since: now},
				Agent:      &state.Agent{Phase: state.PhaseStarting, LivenessAt: now},
			},
		},
		{
			name: "stopping",
			in: Inputs{
				Controller: &state.Controller{
					Status:   state.StatusStopping,
					Endpoint: state.Endpoint{IP: "203.0.113.7", GamePort: 16261},
					Price:    state.Price{USDPerHour: 0.0213},
					Since:    now,
				},
				Agent: &state.Agent{PlayersCount: 0, PlayersAt: now, LivenessAt: now},
			},
		},
		{
			name: "offline",
			in: Inputs{
				Controller: &state.Controller{
					Status:        state.StatusOffline,
					RestoreTarget: "backup_20260819_013623.zip",
					Since:         now,
				},
			},
		},
		{
			name: "failed",
			in: Inputs{
				Controller: &state.Controller{
					Status:    state.StatusFailed,
					LastError: "no bids within 300s (11 providers asked)",
					Since:     now,
				},
			},
		},
		{
			// No documents at all: a controller that has just started and has not
			// read the branch yet. v1 rendered a JSON error here.
			name: "no-state",
			in:   Inputs{},
		},
	}

	// The backups page gets its own axis: the list, the disk warning, and both
	// unlock states, since the locked page is what a player sees.
	backups := &state.Backups{Items: []state.Backup{
		{
			Name: "backup_20260820_013623.zip", Size: 7 * 1024 * 1024,
			CreatedAt: now, SHA256: strings.Repeat("a", 64),
		},
		{
			Name: "backup_20260819_013623.zip", Size: 3 * 1024 * 1024,
			CreatedAt:    state.Stamp{Time: now.Time.Add(-24 * time.Hour)},
			DownloadedAt: now, SHA256: strings.Repeat("b", 64),
		},
	}}

	// decorate adds the parts every page carries regardless of status, so the one
	// axis a case varies is the one its name claims.
	decorate := func(in Inputs) Inputs {
		in.Version = "42.20.3"
		in.Packages = Packages{
			Client: PackageStats{Mods: 12, Files: 340, Size: 134 << 20},
			Common: PackageStats{Mods: 12, Files: 18, Size: 2 << 20},
			Server: PackageStats{Files: 26, Size: 96 << 10},
		}
		in.Guide = galleryGuide
		in.Backups = backups
		in.DiskUsedPercent = 42
		return in
	}

	var written []string
	for _, c := range cases {
		for _, lang := range Langs {
			in := decorate(c.in)
			// Both unlock states: locked is what a player sees, and it is the half
			// carrying the lock and the modal.
			written = append(written,
				render(t, dir, o, in, Unlocked{}, lang, PathHub, c.name+"-locked"),
				render(t, dir, o, in, Unlocked{ServerFiles: true, Backups: true}, lang, PathHub, c.name+"-unlocked"))
		}
	}

	// The backups page does not vary by status, so it varies by the things it does:
	// the unlock, the disk warning, and having nothing to list.
	offline := Inputs{Controller: &state.Controller{Status: state.StatusOffline, Since: now}}
	for _, lang := range Langs {
		full := decorate(offline)
		full.DiskUsedPercent = 91 // over disk_warn_percent: the page starts asking
		written = append(written,
			render(t, dir, o, full, Unlocked{}, lang, PathBackups, "backups-locked"),
			render(t, dir, o, full, Unlocked{Backups: true}, lang, PathBackups, "backups-warning"))

		empty := decorate(offline)
		empty.Backups = &state.Backups{}
		empty.DiskUsedPercent = 3
		written = append(written,
			render(t, dir, o, empty, Unlocked{Backups: true}, lang, PathBackups, "backups-empty"))

		// The two rejection appearances, one per realm. Only reachable from the
		// unlock redirect, so only reachable here through the query.
		written = append(written,
			render(t, dir, o, full, Unlocked{}, lang, PathBackups, "backups-wrong-password",
				"&unlock=wrong&realm=backups"),
			render(t, dir, o, full, Unlocked{}, lang, PathHub, "hub-wrong-password",
				"&unlock=wrong&realm=server-files"))
	}

	if err := writeIndex(dir, written); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d pages written to %s — open index.html", len(written), dir)
}

// galleryGuide is markdown exercising every construct the renderer supports, so
// the diff covers the guide as well as the chrome.
const galleryGuide = `## Как подключиться

1. Скачайте клиент по ссылке выше.
2. Распакуйте в отдельную папку — **не** поверх старой сборки.
3. Запустите ` + "`ProjectZomboid64.exe`" + ` и добавьте сервер.

> Пароль от сервера спрашивайте в чате.

- Моды ставятся автоматически
- Сохранения общие

[Вики карты](https://map.projectzomboid.com/)
`

// galleryData serves the inputs exactly as given. fakeData is not reusable here:
// it replaces Guide with a marker so the handler tests can prove the locale
// reached it, and in a gallery the markdown itself is what is under review.
type galleryData struct {
	in   Inputs
	open Unlocked
}

func (g *galleryData) Snapshot(Lang) Inputs            { return g.in }
func (g *galleryData) Unlocked(*http.Request) Unlocked { return g.open }
func (g *galleryData) Unlock(http.ResponseWriter, *http.Request, string, string) bool {
	return false
}

// render writes one page and returns its filename. extra is appended to the query,
// which is how the unlock-failed appearance is reached: it is a state the page only
// enters from a redirect, and it is the one part of the port with no v1 equivalent.
func render(t *testing.T, dir string, o Options, in Inputs, open Unlocked,
	lang Lang, path, name string, extra ...string) string {

	t.Helper()
	h := newTestHandler(t, o, &galleryData{in: in, open: open})

	w := do(h, http.MethodGet, path+"?lang="+string(lang)+strings.Join(extra, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: GET = %d\n%s", name, lang, w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The canaries from the smoke test, because a gallery nobody checks would
	// happily contain them.
	for _, canary := range []string{"ZgotmplZ", "%!", "{{", "<no value>"} {
		if strings.Contains(body, canary) {
			t.Errorf("%s %s: rendered output contains %q", name, lang, canary)
		}
	}

	file := fmt.Sprintf("%s.%s.html", name, lang)
	// The pages link /assets/dashboard.css; written beside them below so the
	// gallery renders with its real stylesheet from a file:// URL.
	body = strings.ReplaceAll(body, `"/assets/`, `"assets/`)
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// writeIndex copies the assets next to the pages and writes a contents page.
func writeIndex(dir string, pages []string) error {
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		return err
	}
	for _, name := range []string{"dashboard.css", "dashboard.js"} {
		b, err := files.ReadFile("assets/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "assets", name), b, 0o644); err != nil {
			return err
		}
	}

	sort.Strings(pages)
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<meta charset=\"utf-8\">\n<title>pzctl dashboard gallery</title>\n")
	b.WriteString("<style>body{font:16px/1.6 system-ui;margin:2rem;max-width:40rem}li{margin:.2rem 0}</style>\n")
	b.WriteString("<h1>pzctl dashboard gallery</h1>\n<ol>\n")
	for _, p := range pages {
		fmt.Fprintf(&b, "<li><a href=%q>%s</a></li>\n", p, strings.TrimSuffix(p, ".html"))
	}
	b.WriteString("</ol>\n")
	return os.WriteFile(filepath.Join(dir, "index.html"), []byte(b.String()), 0o644)
}
