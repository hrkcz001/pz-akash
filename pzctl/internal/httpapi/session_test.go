package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func unlockable(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, harnessOptions{
		sessionTTL:     12 * time.Hour,
		unlockAttempts: 3,
		unlockWindow:   5 * time.Minute,
	})
}

// req builds a bare request for the guard-level tests. It never goes over a
// socket: what is being checked is which credentials the guard accepts, and a
// literal request is the shortest way to hand it one.
func req(cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/server.zip", nil)
	r.RemoteAddr = "203.0.113.9:51234"
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

// cookieFrom pulls one Set-Cookie out of a recorder, as a cookie ready to send
// back.
func cookieFrom(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", name, w.Header().Values("Set-Cookie"))
	return nil
}

// The point of the whole file: an unlock has to open the actual download, not just
// the page's opinion of it. A browser cannot send an Authorization header, so
// before this the guarded links on the dashboard led to a 401.
func TestUnlockOpensTheGuardedDownload(t *testing.T) {
	h := unlockable(t)
	makeZip(t, filepath.Join(h.packages, "server.zip"), []zipEntry{
		{name: "Server/vsrania.ini", body: "Password=__PZ_JOIN_PASSWORD__\n"},
	})

	w := httptest.NewRecorder()
	if !h.srv.Unlock(w, req(), RealmServerFiles, "server-files-token") {
		t.Fatal("the configured password was refused")
	}
	c := cookieFrom(t, w, "pz_unlock_server-files")

	if !h.srv.Unlocked(RealmServerFiles, req(c)) {
		t.Fatal("a request carrying the unlock cookie was still locked")
	}
	// And through the router, which is what a browser actually does next.
	resp := h.do(http.MethodGet, "/server.zip", "", nil, [2]string{"Cookie", c.Name + "=" + c.Value})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /server.zip with the cookie = %d, want 200", resp.StatusCode)
	}

	// The cookie is not the password, which is the entire reason it exists: v1 put
	// the password in the URL of every guarded link.
	if strings.Contains(c.Value, "server-files-token") {
		t.Fatal("the cookie carries the password")
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Fatalf("cookie flags = HttpOnly:%v SameSite:%v Path:%q", c.HttpOnly, c.SameSite, c.Path)
	}
}

func TestUnlockRefusesTheWrongPassword(t *testing.T) {
	h := unlockable(t)

	for _, pw := range []string{"", "server-files-toke", "server-files-token ", "backups-token", "SERVER-FILES-TOKEN"} {
		w := httptest.NewRecorder()
		if h.srv.Unlock(w, req(), RealmServerFiles, pw) {
			t.Fatalf("password %q was accepted", pw)
		}
		if len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("password %q set a cookie: %v", pw, w.Header().Values("Set-Cookie"))
		}
	}
}

// Two realms, two secrets, two cookies. An unlock of one must not carry into the
// other — the signature covers the realm name for exactly this.
func TestUnlockDoesNotCrossRealms(t *testing.T) {
	h := unlockable(t)

	w := httptest.NewRecorder()
	if !h.srv.Unlock(w, req(), RealmBackups, "backups-token") {
		t.Fatal("the backups password was refused")
	}
	backups := cookieFrom(t, w, "pz_unlock_backups")

	if !h.srv.Unlocked(RealmBackups, req(backups)) {
		t.Fatal("the backups cookie did not unlock backups")
	}
	if h.srv.Unlocked(RealmServerFiles, req(backups)) {
		t.Fatal("the backups cookie unlocked the server files")
	}
	// Nor does renaming it, which is what a signature over the realm buys.
	renamed := &http.Cookie{Name: "pz_unlock_server-files", Value: backups.Value}
	if h.srv.Unlocked(RealmServerFiles, req(renamed)) {
		t.Fatal("a backups cookie renamed to the other realm was accepted")
	}
}

func TestForgedAndExpiredCookiesAreRefused(t *testing.T) {
	h := unlockable(t)
	sess := h.srv.sessions()
	if sess == nil {
		t.Fatal("no sessions were configured")
	}

	good := sess.mint(RealmBackups, h.clk.now().Add(time.Hour))
	if !sess.valid(RealmBackups, req(&http.Cookie{Name: "pz_unlock_backups", Value: good})) {
		t.Fatal("a freshly minted cookie was refused")
	}

	exp, mac, _ := strings.Cut(good, ".")
	bad := map[string]string{
		"no signature":       exp,
		"empty":              "",
		"signature only":     mac,
		"unsigned expiry":    exp + ".",
		"flipped signature":  exp + "." + flip(mac),
		"expiry moved out":   forgeExpiry(mac),
		"not a number":       "tomorrow." + mac,
		"padded number":      " " + exp + "." + mac,
		"another key's mac":  exp + "." + strings.Repeat("A", len(mac)),
		"trailing separator": good + ".",
	}
	for name, v := range bad {
		if sess.valid(RealmBackups, req(&http.Cookie{Name: "pz_unlock_backups", Value: v})) {
			t.Errorf("%s (%q) was accepted", name, v)
		}
	}

	// A real cookie stops working when its expiry passes, which is what session_ttl
	// is for: the signing key alone would make one valid until the process restarts.
	h.clk.advance(2 * time.Hour)
	if sess.valid(RealmBackups, req(&http.Cookie{Name: "pz_unlock_backups", Value: good})) {
		t.Fatal("an expired cookie was accepted")
	}
}

// forgeExpiry keeps a valid signature and moves the expiry it was signed over,
// which is the attack the signature has to cover rather than merely sit beside.
func forgeExpiry(mac string) string {
	far := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	return strconv.FormatInt(far, 10) + "." + mac
}

func flip(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// v1's /api/verify was an unauthenticated, unthrottled password oracle that
// answered as fast as it could be asked. Three wrong answers is now the budget,
// and the fourth attempt is refused whether or not it is right.
func TestUnlockRateLimitsPerClient(t *testing.T) {
	h := unlockable(t) // 3 attempts per 5 minutes
	from := func(ip string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/unlock", nil)
		r.RemoteAddr = ip + ":40000"
		return r
	}

	for i := range 3 {
		if h.srv.Unlock(httptest.NewRecorder(), from("203.0.113.1"), RealmBackups, "wrong") {
			t.Fatalf("attempt %d: a wrong password was accepted", i)
		}
	}
	// The budget is spent, so even the right password is refused. Answering the
	// same "no" is deliberate: a distinguishable "throttled" tells a guesser
	// exactly when to pause.
	if h.srv.Unlock(httptest.NewRecorder(), from("203.0.113.1"), RealmBackups, "backups-token") {
		t.Fatal("the fourth attempt was served despite the limit")
	}
	if !hasLog(h, "attempt limit reached") {
		t.Fatalf("nothing in the log says the limit was hit: %v", h.logs)
	}

	// Somebody else's attempts are their own. Without this, one wrong guess would
	// lock every player out of the downloads.
	if !h.srv.Unlock(httptest.NewRecorder(), from("203.0.113.2"), RealmBackups, "backups-token") {
		t.Fatal("a different client was caught by the first one's limit")
	}

	// And the window passes.
	h.clk.advance(6 * time.Minute)
	if !h.srv.Unlock(httptest.NewRecorder(), from("203.0.113.1"), RealmBackups, "backups-token") {
		t.Fatal("the limit did not lift after its window")
	}
}

// The per-client key comes from a header the client can forge, which is the right
// trade behind a proxy (see clientKey) but would be an unmetered oracle on its own.
// The global counter is what closes that: relabelling attempts spreads them across
// buckets and still spends the shared budget.
func TestUnlockRateLimitIsAlsoGlobal(t *testing.T) {
	h := unlockable(t) // 3 per client, so 60 globally
	spoof := func(i int) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/unlock", nil)
		r.RemoteAddr = "203.0.113.1:40000"
		r.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i%256))
		return r
	}

	tried, served := 0, 0
	for i := range 200 {
		tried++
		if h.srv.Unlock(httptest.NewRecorder(), spoof(i), RealmBackups, "wrong") {
			t.Fatal("a wrong password was accepted")
		}
		if !strings.Contains(lastLog(h), "attempt limit reached") {
			served++
		}
	}
	if served > 60 {
		t.Fatalf("%d of %d spoofed attempts were compared, want at most the global budget of 60", served, tried)
	}
	if served == 0 {
		t.Fatal("no attempt was compared at all — the limiter is refusing everything")
	}
}

// A realm with no configured password denies both credentials. The opposite
// default is how v1 ended up serving /server.zip unauthenticated whenever the env
// var failed to reach the container.
func TestUnlockWithNoConfiguredPasswordAlwaysFails(t *testing.T) {
	h := newHarness(t, harnessOptions{
		noSecrets:      true,
		sessionTTL:     time.Hour,
		unlockAttempts: 100,
		unlockWindow:   time.Minute,
	})
	for _, pw := range []string{"", "backups-token", "anything"} {
		if h.srv.Unlock(httptest.NewRecorder(), req(), RealmBackups, pw) {
			t.Fatalf("password %q unlocked a realm with no secret", pw)
		}
	}
	// Not even a cookie this process signed, because there was nothing to verify
	// before signing it.
	sess := h.srv.sessions()
	v := sess.mint(RealmBackups, h.clk.now().Add(time.Hour))
	if h.srv.Unlocked(RealmBackups, req(&http.Cookie{Name: "pz_unlock_backups", Value: v})) {
		t.Fatal("a signed cookie unlocked a realm with no secret")
	}
}

func TestUnlockRejectsRealmsThatAreNotRealms(t *testing.T) {
	h := unlockable(t)
	for _, realm := range []Realm{RealmPublic, "admin", "BACKUPS", "backups ", "../backups", "server_files"} {
		if h.srv.Unlock(httptest.NewRecorder(), req(), realm, "backups-token") {
			t.Fatalf("realm %q was unlocked", realm)
		}
	}
}

// No dashboard configured means no unlock machinery at all: the bearer token is
// the only credential, which is the shape every other test in this package uses.
func TestWithoutASessionTTLThereIsNoCookieAuth(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if h.srv.sessions() != nil {
		t.Fatal("sessions were built with no session_ttl configured")
	}
	if h.srv.Unlock(httptest.NewRecorder(), req(), RealmBackups, "backups-token") {
		t.Fatal("an unlock succeeded with no sessions configured")
	}
	// And the header still works, so turning the dashboard off does not lock the
	// agent out.
	r := req()
	SetAuth(r, "backups-token")
	if !h.srv.Unlocked(RealmBackups, r) {
		t.Fatal("the bearer token stopped working")
	}
}

// The Secure flag has to follow the visitor's scheme rather than the container's:
// the controller only ever speaks plain HTTP, so r.TLS is nil on every real
// request and a hardcoded Secure would make the unlock silently not stick.
func TestUnlockCookieSecureFollowsTheForwardedScheme(t *testing.T) {
	h := unlockable(t)
	cases := map[string]bool{"": false, "http": false, "https": true, "HTTPS": true, "https, http": true}
	for proto, want := range cases {
		r := req()
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		w := httptest.NewRecorder()
		if !h.srv.Unlock(w, r, RealmBackups, "backups-token") {
			t.Fatalf("proto %q: the password was refused", proto)
		}
		if got := cookieFrom(t, w, "pz_unlock_backups").Secure; got != want {
			t.Errorf("X-Forwarded-Proto %q: Secure = %v, want %v", proto, got, want)
		}
	}
}

func hasLog(h *harness, substr string) bool {
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func lastLog(h *harness) string {
	if len(h.logs) == 0 {
		return ""
	}
	return h.logs[len(h.logs)-1]
}
