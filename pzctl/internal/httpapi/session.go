package httpapi

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cookieName is the per-realm unlock cookie. The realm is in the name so the two
// unlocks are independent, and in the signature so a cookie renamed by hand does
// not validate against the other realm.
func cookieName(realm Realm) string { return "pz_unlock_" + string(realm) }

// sessions turns "knows the realm password" into "carries a cookie", so a browser
// can follow a guarded download link.
//
// This exists because the guard's other credential is an Authorization header,
// which a browser following an <a href> cannot send. v1 solved the same problem by
// putting the password in the query string of every link, which put it in browser
// history, in the Referer of anything the page linked to, and in the access log of
// whatever sat in front of the container.
//
// There is no session table. The cookie is an expiry and an HMAC over it, so
// verification is a signature check and nothing is stored per visitor — no memory
// to exhaust by asking for unlocks, and no state to lose. The signing key is
// generated at startup, which means a controller restart ends every session; that
// is a re-entry of a password an operator already has, and it removes the question
// of what a leaked cookie is still worth after a redeploy.
type sessions struct {
	key []byte
	ttl time.Duration
	now func() time.Time

	// The limiter. Attempts are only counted when they fail, so the operator who
	// unlocks both realms in a row is not most of the way to being locked out.
	limit  int
	window time.Duration

	mu      sync.Mutex
	clients map[string]*counter
	all     counter
}

// counter is a fixed-window attempt counter. A sliding window would be more even,
// but this is guarding a random 32-character secret against online guessing: the
// job is to turn "as fast as the network allows" into "a handful per window", and
// the seam at the window edge does not change that.
type counter struct {
	count int
	until time.Time
}

// maxClients bounds the limiter's memory. Each entry is keyed by a client-supplied
// address, so without a cap a spoofed X-Forwarded-For per request would be an
// allocation per request. At the cap, expired entries are dropped and — if every
// entry is still live — new keys fall back to the global counter, which is the
// safe direction: an attacker can fill the table but cannot use it to get an
// unmetered attempt.
const maxClients = 4096

// newSessions returns nil when the dashboard is not configured, so a controller
// running without one carries no unlock machinery at all.
func newSessions(ttl, unlockWindow time.Duration, attempts int, now func() time.Time) (*sessions, error) {
	if ttl <= 0 {
		return nil, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// Unreachable on any supported platform, and fatal if it happens: a
		// predictable key is a forgeable unlock, so this must not fall back to
		// something derived from the clock.
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if attempts <= 0 {
		attempts = 5
	}
	if unlockWindow <= 0 {
		unlockWindow = 5 * time.Minute
	}
	return &sessions{
		key:     key,
		ttl:     ttl,
		now:     now,
		limit:   attempts,
		window:  unlockWindow,
		clients: map[string]*counter{},
	}, nil
}

// grant sets the cookie for realm on w.
//
// HttpOnly because no script needs to read it and the page has none that would.
// SameSite=Lax rather than Strict: a player follows a link to the dashboard from
// chat, and Strict would present them a locked page on arrival even though they
// unlocked it minutes ago. Lax still withholds the cookie from cross-site POSTs,
// which is the case that matters here.
func (s *sessions) grant(w http.ResponseWriter, r *http.Request, realm Realm) {
	if s == nil {
		return
	}
	exp := s.now().Add(s.ttl)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(realm),
		Value:    s.mint(realm, exp),
		Path:     "/",
		MaxAge:   int(s.ttl.Seconds()),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// revoke clears the cookie for realm. Nothing calls it yet — there is no lock
// button on the page — but the cookie is set with a Path and a SameSite that a
// hand-written expiry would have to match exactly, so the pairing lives here.
func (s *sessions) revoke(w http.ResponseWriter, r *http.Request, realm Realm) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(realm),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// valid reports whether r carries an unexpired cookie for realm.
func (s *sessions) valid(realm Realm, r *http.Request) bool {
	if s == nil {
		return false
	}
	c, err := r.Cookie(cookieName(realm))
	if err != nil || c.Value == "" {
		return false
	}
	raw, mac, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	unix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	// Signature first, expiry second. The other order would answer "is this a
	// real cookie that has expired" differently from "is this a forgery", and the
	// difference is observable in the timing.
	want := s.sign(realm, raw)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return false
	}
	return s.now().Before(time.Unix(unix, 0))
}

// mint builds a cookie value: the expiry, and a signature over it.
func (s *sessions) mint(realm Realm, exp time.Time) string {
	raw := strconv.FormatInt(exp.Unix(), 10)
	return raw + "." + s.sign(realm, raw)
}

func (s *sessions) sign(realm Realm, raw string) string {
	m := hmac.New(sha256.New, s.key)
	// The separator is not a valid character in either half, so no pair of
	// (realm, expiry) values can produce the same signed string as another —
	// the canonicalisation bug that lets a signature be reused across realms.
	m.Write([]byte(string(realm) + "\x00" + raw))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// --- the attempt limiter ---

// take reports whether another password attempt from r is permitted.
//
// Two counters have to allow it: one for this client and one for the whole
// process. The per-client counter is the useful one, and it is keyed on an address
// the client can influence (see clientKey). The global counter is what makes that
// acceptable — spoofing the header buys a fresh per-client budget but still spends
// the global one, so the total rate is bounded no matter how the attempts are
// labelled.
func (s *sessions) take(r *http.Request) bool {
	if s == nil {
		return false
	}
	now := s.now()
	key := clientKey(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.all.allows(now, s.limit*20, s.window) {
		return false
	}
	c, ok := s.clients[key]
	if !ok {
		return true // no failures recorded against this client yet
	}
	return c.allows(now, s.limit, s.window)
}

// penalise counts one failed attempt.
func (s *sessions) penalise(r *http.Request) {
	if s == nil {
		return
	}
	now := s.now()
	key := clientKey(r)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.all.add(now, s.window)

	if c, ok := s.clients[key]; ok {
		c.add(now, s.window)
		return
	}
	if len(s.clients) >= maxClients {
		s.evict(now)
	}
	if len(s.clients) >= maxClients {
		// Every entry is still live. Dropping this one on the floor is the right
		// failure: the global counter above already recorded the attempt, so the
		// rate stays bounded, and no allocation is made on a client's say-so.
		return
	}
	c := &counter{}
	c.add(now, s.window)
	s.clients[key] = c
}

// allows reports whether a counter is under limit, resetting it when its window
// has passed.
func (c *counter) allows(now time.Time, limit int, d time.Duration) bool {
	if !now.Before(c.until) {
		c.count, c.until = 0, now.Add(d)
	}
	return c.count < limit
}

func (c *counter) add(now time.Time, d time.Duration) {
	if !now.Before(c.until) {
		c.count, c.until = 0, now.Add(d)
	}
	c.count++
}

// evict drops entries whose window has passed. Called under s.mu.
func (s *sessions) evict(now time.Time) {
	for k, c := range s.clients {
		if !now.Before(c.until) {
			delete(s.clients, k)
		}
	}
}

// clientKey is what the per-client limiter counts against.
//
// It prefers the left-most X-Forwarded-For entry, which a client can forge. That
// is a deliberate trade, and the alternative is worse: with Cloudflare or any
// other proxy in front, RemoteAddr is the proxy for every visitor at once, so five
// wrong guesses from one person would lock every player out of the downloads for
// the whole window. Forging the header splits an attacker's attempts across
// buckets, which the global counter in take already bounds; keying on RemoteAddr
// would turn the limiter itself into the denial of service.
func clientKey(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, _ := strings.Cut(fwd, ","); strings.TrimSpace(first) != "" {
			// Capped because the header is arbitrary client input and this string
			// becomes a map key.
			v := strings.TrimSpace(first)
			if len(v) > 64 {
				v = v[:64]
			}
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestIsHTTPS reports whether the visitor's connection to the edge was
// encrypted, which is what decides the Secure flag.
//
// The forwarded header is trusted because the controller terminates plain HTTP
// inside Akash and TLS is somebody else's job in front of it, so r.TLS is nil on
// every real request. Getting this wrong in the trusting direction sets Secure on
// a cookie served over HTTP, which makes the unlock silently not stick; the
// untrusting direction would ship the cookie without Secure over a TLS connection.
// Neither is a credential leak — the cookie is not the password — and the first is
// the more visible failure, so the header wins.
func requestIsHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if first, _, ok := strings.Cut(proto, ","); ok {
		proto = first
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
