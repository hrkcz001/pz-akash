package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// fakeData stands in for the controller side of the Data interface, and records
// what it was asked so the tests can assert the locale reached it.
type fakeData struct {
	in   Inputs
	open Unlocked

	// accept is the only password Unlock takes. Empty accepts none, which is what
	// a controller whose secrets did not arrive looks like.
	accept string

	asked []Lang   // every locale Snapshot was asked for, in order
	tried []string // every realm Unlock was asked about
}

func (f *fakeData) Snapshot(lang Lang) Inputs {
	f.asked = append(f.asked, lang)
	in := f.in
	in.Guide = "guide for " + string(lang)
	return in
}

func (f *fakeData) Unlocked(*http.Request) Unlocked { return f.open }

func (f *fakeData) Unlock(w http.ResponseWriter, r *http.Request, realm, password string) bool {
	f.tried = append(f.tried, realm)
	if f.accept == "" || password != f.accept {
		return false
	}
	http.SetCookie(w, &http.Cookie{Name: "pz_" + realm, Value: "ok", Path: "/"})
	return true
}

func newTestHandler(t *testing.T, o Options, d Data) *Handler {
	t.Helper()
	h, err := NewHandler(HandlerOptions{View: o, Data: d, Logf: t.Logf})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func do(h http.Handler, method, target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// fullOptions turns on every optional block, so the smoke test walks the branches
// a minimal config would skip.
func fullOptions(t *testing.T) Options {
	t.Helper()
	o := testOptions(t)
	o.TorrentURL = "/torrents/client.torrent"
	o.JoinPassword = "hunter2"
	// Both version badges. They are {{with}} blocks, which means a minimal config
	// renders neither — and a {{.Typo}} inside a branch nothing takes is invisible
	// until the day an operator sets the value.
	o.Version = "v1.0.0"
	o.ServerVersion = "42.20.3"
	return o
}

// The smoke test, and the only one that proves the templates execute at all: a
// {{.Typo}} is a runtime error in html/template, so a view struct that drifted
// from its markup compiles, passes every view-model test, and 500s in production.
// Four stages × two locales × four unlock combinations × both pages.
func TestBothPagesRenderForEveryStageAndLocale(t *testing.T) {
	o := fullOptions(t)
	now := state.Now(o.Loc)

	controllers := map[string]*state.Controller{
		"online":   onlineController(),
		"booting":  {Status: state.StatusBooting},
		"stopping": {Status: state.StatusStopping, Price: state.Price{USDPerHour: 0.4}},
		"offline":  {Status: state.StatusOffline, RestoreTarget: "backup_20260819_013623.zip"},
		"nil":      nil,
	}
	unlocks := map[string]Unlocked{
		"locked":  {},
		"server":  {ServerFiles: true},
		"backups": {Backups: true},
		"both":    {ServerFiles: true, Backups: true},
	}

	base := Inputs{
		GameVersion: "42.20.3",
		Packages: Packages{
			Client: PackageStats{Mods: 12, Files: 340, Size: 134 * 1024 * 1024},
			Common: PackageStats{Files: 4, Size: 2048},
			Server: PackageStats{},
		},
		Backups: &state.Backups{Items: []state.Backup{
			{Name: "backup_20260819_013623.zip", Size: 3 * 1024 * 1024, CreatedAt: now, DownloadedAt: now},
			{Name: "backup_20260820_013623.zip", Size: 7 * 1024 * 1024, CreatedAt: now},
		}},
		Agent:           &state.Agent{PlayersCount: 3, PlayersAt: now},
		DiskUsedPercent: 91,
	}

	for _, lang := range Langs {
		for cname, ctl := range controllers {
			for uname, open := range unlocks {
				in := base
				in.Controller = ctl
				d := &fakeData{in: in, open: open}
				h := newTestHandler(t, o, d)

				for _, path := range []string{PathConnect, PathBackups} {
					name := string(lang) + "/" + cname + "/" + uname + path
					w := do(h, http.MethodGet, path+"?lang="+string(lang))
					if w.Code != http.StatusOK {
						t.Fatalf("%s: GET = %d\n%s", name, w.Code, w.Body.String())
					}
					assertWholeDocument(t, name, lang, w)
				}
			}
		}
	}
}

// assertWholeDocument checks the properties that distinguish a rendered page from
// a half-rendered one, which a 200 by itself does not.
func assertWholeDocument(t *testing.T, name string, lang Lang, w *httptest.ResponseRecorder) {
	t.Helper()
	body := w.Body.String()

	if !strings.HasPrefix(body, "<!DOCTYPE html>") {
		t.Errorf("%s: body does not start with a doctype: %.60q", name, body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("%s: body does not end with </html>: %.60q", name, tail(body))
	}
	if !strings.Contains(body, `<html lang="`+string(lang)+`">`) {
		t.Errorf("%s: rendered in the wrong locale", name)
	}
	// html/template writes ZgotmplZ in place of a URL it could not prove safe, and
	// fmt writes %! for a botched verb. Both render as a visible page, so only a
	// test looking for them catches either.
	for _, canary := range []string{"ZgotmplZ", "%!", "{{", "<no value>"} {
		if strings.Contains(body, canary) {
			t.Errorf("%s: rendered output contains %q", name, canary)
		}
	}
	// No credential in any URL. v1 put the backups password in the query string of
	// every download link, the form action and the language switcher.
	for _, leak := range []string{"token=", "password=", "?pwd"} {
		if strings.Contains(body, leak) {
			t.Errorf("%s: a URL on the page carries %q", name, leak)
		}
	}
	if got, want := w.Header().Get("Content-Length"), strconv.Itoa(len(body)); got != want {
		t.Errorf("%s: Content-Length %q, body is %s bytes", name, got, want)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("%s: Cache-Control = %q; a cached page is a lie about whether anyone can connect", name, got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("%s: Content-Type = %q", name, got)
	}
}

func tail(s string) string {
	if len(s) > 60 {
		return s[len(s)-60:]
	}
	return s
}

// HEAD has to agree with GET about the length, or a client that trusts it reads
// the wrong number of bytes.
func TestHeadIsAGetWithoutTheBody(t *testing.T) {
	o := fullOptions(t)
	d := &fakeData{in: Inputs{Controller: onlineController()}}
	h := newTestHandler(t, o, d)

	for _, path := range []string{PathConnect, PathBackups, PathStatus} {
		body := do(h, http.MethodGet, path).Body.Len()
		head := do(h, http.MethodHead, path)
		if head.Code != http.StatusOK {
			t.Errorf("HEAD %s = %d", path, head.Code)
		}
		if head.Body.Len() != 0 {
			t.Errorf("HEAD %s returned %d bytes of body", path, head.Body.Len())
		}
		if got, want := head.Header().Get("Content-Length"), strconv.Itoa(body); got != want {
			t.Errorf("HEAD %s: Content-Length %q, GET body is %s", path, got, want)
		}
	}
}

func TestPagesRejectOtherMethods(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})

	cases := map[string]string{
		PathRoot:    "GET, HEAD",
		PathConnect: "GET, HEAD",
		PathBackups: "GET, HEAD",
		PathStatus:  "GET, HEAD",
		PathUnlock:  "POST",
	}
	for path, allow := range cases {
		method := http.MethodPost
		if path == PathUnlock {
			method = http.MethodGet
		}
		w := do(h, method, path)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", method, path, w.Code)
		}
		if got := w.Header().Get("Allow"); got != allow {
			t.Errorf("%s: Allow = %q, want %q", path, got, allow)
		}
	}
}

func TestUnknownPathsAre404(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})
	// /backups/<name> belongs to httpapi. The dashboard must not answer for it, or
	// mounting it under "/" would shadow every download.
	//
	// /packages is in the list because the tab used to be called that, and the
	// rename deliberately left no alias behind: one name for the page, so a link
	// given out loud goes where the tab says it does.
	for _, path := range []string{
		"/nope", "/backups/backup_20260819_013623.zip", "/server.zip", "/api/", "/assets", "/packages",
	} {
		if w := do(h, http.MethodGet, path); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

// The main page moved off the root, and the root is what a player types or pastes
// into a chat. A 404 there would be the whole dashboard missing for everyone who
// does not already have the deep link.
func TestRootRedirectsToConnect(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})

	cases := map[string]string{
		PathRoot:                                 PathConnect,
		PathRoot + "?lang=ru":                    PathConnect + "?lang=ru",
		PathRoot + "?unlock=wrong&realm=backups": PathConnect + "?unlock=wrong&realm=backups",
	}
	for from, want := range cases {
		w := do(h, http.MethodGet, from)
		// 302, not 301: a permanent redirect is cached until the reader clears it,
		// and this route has already moved once.
		if w.Code != http.StatusFound {
			t.Errorf("GET %s = %d, want 302", from, w.Code)
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("GET %s: Location = %q, want %q", from, got, want)
		}
	}
}

// The tab and the page have to agree about the name. Both halves are template
// literals that nothing else reads, so a rename that touched one and not the other
// would render a nav entry that is never marked, or one that links to a 404 — and
// neither shows up as a failure anywhere else in this package.
func TestNavMarksTheConnectTabOnItsOwnPage(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{in: Inputs{Controller: onlineController()}})

	connect := do(h, http.MethodGet, PathConnect).Body.String()
	if !strings.Contains(connect, `<a href="/connect" class="nav-item active"`) {
		t.Errorf("the connect tab is not marked active on %s", PathConnect)
	}
	if backups := do(h, http.MethodGet, PathBackups).Body.String(); !strings.Contains(
		backups, `<a href="/connect" class="nav-item"`) {
		t.Errorf("the connect tab is marked active on %s, or does not link to %s", PathBackups, PathConnect)
	}
}

// --- status poll ---

// The poll returns finished strings for the locale that asked, because the page
// and the poll have to agree and only one of them can own the wording.
func TestStatusPollReturnsRenderedStringsForTheRequestedLocale(t *testing.T) {
	o := fullOptions(t)
	in := Inputs{Controller: onlineController(), Agent: &state.Agent{PlayersCount: 3, PlayersAt: state.Now(o.Loc)}}
	h := newTestHandler(t, o, &fakeData{in: in})

	for _, lang := range Langs {
		w := do(h, http.MethodGet, PathStatus+"?lang="+string(lang))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: GET %s = %d", lang, PathStatus, w.Code)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("%s: Content-Type = %q", lang, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q", lang, got)
		}

		var got Status
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: decoding %s: %v", lang, w.Body.String(), err)
		}
		// Snapshot is called again, so this is the same comparison the page makes,
		// not a re-read of a cached render.
		if want := BuildStatus(o, in, lang); got != want {
			t.Errorf("%s: poll = %+v, want %+v", lang, got, want)
		}
		if got.Badge.Text == "" || got.Players.Text == "" {
			t.Errorf("%s: the poll returned a payload the browser cannot display: %+v", lang, got)
		}
	}
}

// The script copies badge.class straight into a class attribute, so the poll must
// never hand it something that is not one of the four.
func TestStatusBadgeClassIsAlwaysOneOfTheFour(t *testing.T) {
	o := fullOptions(t)
	statuses := []state.Status{
		state.StatusOnline, state.StatusBackingUp, state.StatusDeploying, state.StatusBooting,
		state.StatusStopping, state.StatusClosing, state.StatusOffline, state.StatusFailed,
	}
	allowed := map[string]bool{"badge-online": true, "badge-booting": true, "badge-stopping": true, "badge-offline": true}

	for _, s := range statuses {
		h := newTestHandler(t, o, &fakeData{in: Inputs{Controller: &state.Controller{Status: s}}})
		var got Status
		if err := json.Unmarshal(do(h, http.MethodGet, PathStatus).Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if !allowed[got.Badge.Class] {
			t.Errorf("%s produced the class %q", s, got.Badge.Class)
		}
		if got.Badge.Dot != strings.TrimPrefix(got.Badge.Class, "badge-") {
			t.Errorf("%s: dot %q disagrees with class %q", s, got.Badge.Dot, got.Badge.Class)
		}
	}
}

// --- unlock ---

func post(h http.Handler, target string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUnlockAcceptsTheRightPassword(t *testing.T) {
	d := &fakeData{accept: "correct horse", in: Inputs{Controller: onlineController()}}
	h := newTestHandler(t, fullOptions(t), d)

	w := post(h, PathUnlock, url.Values{
		"realm":    {"backups"},
		"next":     {PathBackups},
		"password": {"correct horse"},
	})

	// 303, so the browser follows with GET: a 307 would re-post the password to the
	// page, and a refresh would send it again.
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, want 303", w.Code)
	}
	if got := w.Header().Get("Location"); got != PathBackups {
		t.Fatalf("Location = %q, want %q", got, PathBackups)
	}
	if strings.Contains(w.Header().Get("Location"), "correct") {
		t.Fatal("the password reached the Location header")
	}
	if len(d.tried) != 1 || d.tried[0] != "backups" {
		t.Fatalf("Unlock was asked about %v", d.tried)
	}
	if len(w.Result().Cookies()) == 0 {
		t.Fatal("a successful unlock set no cookie, so the next request is locked again")
	}
}

// A wrong password is a redirect back to the page with a marker, and the page
// renders the refusal itself. v1's /api/verify answered the question directly,
// which made it an unauthenticated and unthrottled password oracle.
func TestWrongPasswordComesBackAsARenderedPage(t *testing.T) {
	o := fullOptions(t)
	d := &fakeData{accept: "correct horse", in: Inputs{Controller: onlineController()}}
	h := newTestHandler(t, o, d)

	w := post(h, PathUnlock, url.Values{"realm": {"backups"}, "next": {PathBackups}, "password": {"battery"}})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("POST = %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != PathBackups+"?unlock=wrong&realm=backups" {
		t.Fatalf("Location = %q", loc)
	}
	if strings.Contains(loc, "battery") {
		t.Fatal("the attempted password reached the Location header")
	}
	if w.Body.Len() != 0 && strings.Contains(w.Body.String(), "battery") {
		t.Fatal("the attempted password reached the redirect body")
	}

	// Following it renders the locked page with the reason on it.
	page := do(h, http.MethodGet, loc)
	if page.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", loc, page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, catalog[RU].WrongPwd) {
		t.Errorf("the backups page carries no refusal message")
	}
	if strings.Contains(body, "backup_") {
		t.Error("a page that refused the password still listed the archives")
	}

	// The hub's version of the same: the modal comes back already open.
	hub := do(h, http.MethodGet, PathConnect+"?unlock=wrong&realm=server-files")
	if !strings.Contains(hub.Body.String(), "modal-backdrop open") {
		t.Error("the hub's modal did not come back open")
	}
	if !strings.Contains(hub.Body.String(), catalog[RU].ModalErrWrong) {
		t.Error("the hub's modal carries no refusal message")
	}
	if !strings.Contains(hub.Body.String(), "modal-error shown") {
		t.Error("the refusal message is in the document but hidden")
	}
}

// A marker nobody redirected has to be inert, or anyone could hand a player a URL
// that renders the password prompt with a refusal already on it.
func TestOnlyTheTwoRealmsAreAccepted(t *testing.T) {
	d := &fakeData{accept: "correct horse"}
	h := newTestHandler(t, fullOptions(t), d)

	for _, realm := range []string{"", "admin", "server-files ", "BACKUPS", "../backups"} {
		w := post(h, PathUnlock, url.Values{"realm": {realm}, "password": {"correct horse"}})
		if w.Code != http.StatusBadRequest {
			t.Errorf("realm %q = %d, want 400", realm, w.Code)
		}
	}
	if len(d.tried) != 0 {
		t.Errorf("Unlock was called for %v; an unknown realm must not reach the auth", d.tried)
	}

	// And the same list on the query-string marker the page reads.
	for _, realm := range []string{"admin", "", "BACKUPS"} {
		body := do(h, http.MethodGet, PathConnect+"?unlock=wrong&realm="+url.QueryEscape(realm)).Body.String()
		if strings.Contains(body, "modal-backdrop open") {
			t.Errorf("realm %q opened the modal", realm)
		}
	}
}

// next is attacker-supplied, so it is matched against the two pages that exist
// rather than sanitised.
func TestUnlockCannotBeTurnedIntoAnOpenRedirect(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{accept: "correct horse"})

	cases := map[string]string{
		"https://evil.example/":  PathBackups,
		"//evil.example/":        PathBackups,
		"/backups/../../etc":     PathBackups,
		"/backups?token=x":       PathBackups,
		"javascript:alert(1)":    PathBackups,
		"":                       PathBackups,
		"http://evil.example/#/": PathBackups,
	}
	for next, want := range cases {
		w := post(h, PathUnlock, url.Values{"realm": {"backups"}, "next": {next}, "password": {"correct horse"}})
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("next=%q redirected to %q, want %q", next, got, want)
		}
	}

	// The realm decides the fallback, so a server-files unlock lands on the hub.
	w := post(h, PathUnlock, url.Values{"realm": {"server-files"}, "next": {"https://evil.example/"}, "password": {"correct horse"}})
	if got := w.Header().Get("Location"); got != PathConnect {
		t.Errorf("Location = %q, want %q", got, PathConnect)
	}
}

// The realm names are in the templates as literals and in httpapi as constants.
// Nothing at build time connects the two, so this is the connection.
func TestRealmNamesMatchTheAPI(t *testing.T) {
	if realmServerFiles != string(httpapi.RealmServerFiles) {
		t.Errorf("realmServerFiles = %q, httpapi has %q", realmServerFiles, httpapi.RealmServerFiles)
	}
	if realmBackups != string(httpapi.RealmBackups) {
		t.Errorf("realmBackups = %q, httpapi has %q", realmBackups, httpapi.RealmBackups)
	}
	// And the string the hub's unlock button posts, which is markup rather than Go.
	body := do(newTestHandler(t, fullOptions(t), &fakeData{}), http.MethodGet, PathConnect).Body.String()
	if !strings.Contains(body, `data-unlock="`+realmServerFiles+`"`) {
		t.Errorf("the locked card does not name the %q realm", realmServerFiles)
	}
}

// --- locale ---

// The order is what a reader expects: what I just clicked, what I chose last time,
// what my browser says. v1 kept the choice in localStorage, so the server never
// knew it and every page arrived in Russian before JavaScript rewrote it.
func TestLocaleComesFromQueryThenCookieThenHeader(t *testing.T) {
	o := fullOptions(t)

	newH := func() (*Handler, *fakeData) {
		d := &fakeData{in: Inputs{Controller: onlineController()}}
		return newTestHandler(t, o, d), d
	}

	t.Run("query wins and is remembered", func(t *testing.T) {
		h, d := newH()
		w := do(h, http.MethodGet, PathConnect+"?lang=en", &http.Cookie{Name: langCookie, Value: "ru"})
		if got := lastAsked(t, d); got != EN {
			t.Fatalf("Snapshot was asked for %q", got)
		}
		if !strings.Contains(w.Body.String(), `<html lang="en">`) {
			t.Error("the page did not render in the locale the query asked for")
		}
		c := cookie(t, w, langCookie)
		if c.Value != "en" {
			t.Fatalf("%s = %q, want en", langCookie, c.Value)
		}
		// A display preference, not a credential — but nothing in the page reads it.
		if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
			t.Errorf("cookie attributes: HttpOnly=%v SameSite=%v Path=%q", c.HttpOnly, c.SameSite, c.Path)
		}
		if c.MaxAge <= 0 {
			t.Errorf("MaxAge = %d; a session cookie asks again every visit", c.MaxAge)
		}
	})

	t.Run("cookie is honoured and not rewritten", func(t *testing.T) {
		h, d := newH()
		w := do(h, http.MethodGet, PathConnect, &http.Cookie{Name: langCookie, Value: "en"})
		if got := lastAsked(t, d); got != EN {
			t.Fatalf("Snapshot was asked for %q", got)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Error("a request that changed nothing set a cookie")
		}
	})

	t.Run("accept-language, including the region form", func(t *testing.T) {
		h, d := newH()
		r := httptest.NewRequest(http.MethodGet, PathConnect, nil)
		r.Header.Set("Accept-Language", "en-GB,en;q=0.9")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := lastAsked(t, d); got != EN {
			t.Fatalf("Snapshot was asked for %q", got)
		}
	})

	t.Run("nonsense falls back to the configured default", func(t *testing.T) {
		h, d := newH()
		do(h, http.MethodGet, PathConnect+"?lang=de", &http.Cookie{Name: langCookie, Value: "klingon"})
		if got := lastAsked(t, d); got != RU {
			t.Fatalf("Snapshot was asked for %q, want the default", got)
		}
	})

	// A cached page keyed on the URL alone would serve one visitor's locale to the
	// next, and Cloudflare sits in front of this.
	t.Run("the response says what it varies on", func(t *testing.T) {
		h, _ := newH()
		for _, path := range []string{PathConnect, PathBackups} {
			if got := do(h, http.MethodGet, path).Header().Get("Vary"); !strings.Contains(got, "Cookie") {
				t.Errorf("%s: Vary = %q", path, got)
			}
		}
	})
}

func lastAsked(t *testing.T, d *fakeData) Lang {
	t.Helper()
	if len(d.asked) == 0 {
		t.Fatal("Snapshot was never called")
	}
	return d.asked[len(d.asked)-1]
}

func cookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, w.Header().Values("Set-Cookie"))
	return nil
}

// TLS is terminated at Cloudflare and the container speaks plain HTTP, so r.TLS is
// nil on a connection that was encrypted the whole way to the reader.
func TestSecureCookieFollowsTheForwardedScheme(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})

	plain := do(h, http.MethodGet, PathConnect+"?lang=en")
	if cookie(t, plain, langCookie).Secure {
		t.Error("Secure set on a plain HTTP connection; the cookie would never come back")
	}

	r := httptest.NewRequest(http.MethodGet, PathConnect+"?lang=en", nil)
	r.Header.Set("X-Forwarded-Proto", "HTTPS")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !cookie(t, w, langCookie).Secure {
		t.Error("Secure not set behind a TLS-terminating proxy")
	}
}

// --- assets ---

// Embedded files carry no ModTime, so without a validator a browser caches them
// for however long it likes and a stylesheet fix does not reach anyone.
func TestAssetsRevalidateOnABuildDigest(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})

	for _, name := range []string{"dashboard.css", "dashboard.js"} {
		path := PathAssets + name
		w := do(h, http.MethodGet, path)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, w.Code)
		}
		if w.Body.Len() == 0 {
			t.Fatalf("GET %s returned nothing", path)
		}
		etag := w.Header().Get("ETag")
		if etag == "" || !strings.HasPrefix(etag, `"`) {
			t.Fatalf("%s: ETag = %q, want a quoted validator", path, etag)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q", path, got)
		}

		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("If-None-Match", etag)
		rev := httptest.NewRecorder()
		h.ServeHTTP(rev, r)
		if rev.Code != http.StatusNotModified {
			t.Errorf("%s: revalidation = %d, want 304", path, rev.Code)
		}
		if rev.Body.Len() != 0 {
			t.Errorf("%s: 304 carried %d bytes", path, rev.Body.Len())
		}
	}

	// One digest over the whole set, so both files share it and it changes with a
	// build rather than per file.
	css := do(h, http.MethodGet, PathAssets+"dashboard.css").Header().Get("ETag")
	js := do(h, http.MethodGet, PathAssets+"dashboard.js").Header().Get("ETag")
	if css != js {
		t.Errorf("per-file ETags %q and %q, want the one build digest", css, js)
	}
}

// Both pages reference both assets, so a rename that only lands in one place is a
// page with no styling and no poll.
func TestPagesReferenceTheAssetsThatExist(t *testing.T) {
	h := newTestHandler(t, fullOptions(t), &fakeData{})
	for _, path := range []string{PathConnect, PathBackups} {
		body := do(h, http.MethodGet, path).Body.String()
		for _, ref := range []string{PathAssets + "dashboard.css", PathAssets + "dashboard.js"} {
			if !strings.Contains(body, ref) {
				t.Errorf("%s does not reference %s", path, ref)
			}
			if do(h, http.MethodGet, ref).Code != http.StatusOK {
				t.Errorf("%s references %s, which does not serve", path, ref)
			}
		}
	}
}

// --- construction ---

func TestNewHandlerRefusesAnIncompleteConfiguration(t *testing.T) {
	if _, err := NewHandler(HandlerOptions{View: testOptions(t)}); err == nil {
		t.Error("a handler with no Data was accepted")
	}
	// A nil location would silently mean UTC, which is the host-clock problem the
	// configured timezone exists to remove.
	if _, err := NewHandler(HandlerOptions{View: Options{Default: RU}, Data: &fakeData{}}); err == nil {
		t.Error("a handler with no timezone was accepted")
	}
	if _, err := NewHandler(HandlerOptions{View: testOptions(t), Data: &fakeData{}}); err != nil {
		t.Errorf("a complete configuration was refused: %v", err)
	}
}
