package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// guard holds the token for each realm and answers one question: may this request
// have this path.
//
// The tokens are stored as digests rather than as strings. Not because that
// defeats a memory dump — the process has to hold the secrets anyway to
// substitute them into server.zip — but because it makes the comparison
// fixed-length, which is what lets the constant-time compare actually be constant
// time. Comparing a 4-character attempt against a 19-character password leaks the
// length through the timing of the length check itself.
type guard struct {
	digests map[Realm][32]byte

	// sess is the browser's way in: a cookie the unlock endpoint sets, checked as
	// a second credential below. Nil when no dashboard is configured, in which
	// case the header is the only credential there is.
	sess *sessions

	logf func(string, ...any)
}

func newGuard(sec *secrets.Set, sess *sessions, logf func(string, ...any)) *guard {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	g := &guard{digests: map[Realm][32]byte{}, sess: sess, logf: logf}
	if sec == nil {
		return g
	}
	for realm, token := range map[Realm]string{
		RealmServerFiles: sec.ServerFilesPassword,
		RealmBackups:     sec.BackupsPassword,
	} {
		if token != "" {
			g.digests[realm] = sha256.Sum256([]byte(token))
		}
	}
	return g
}

// allow reports whether r carries a credential for realm — either the bearer
// token, which is how the agent authenticates, or an unlock cookie, which is how a
// browser does.
//
// A realm with no configured token denies both. The opposite default — no password
// means no check — is how v1's storage server ended up serving /server.zip
// unauthenticated whenever the env var failed to reach the container, which is a
// state nobody could observe from the outside because a working download looks
// identical either way. The cookie is checked against the same condition for the
// same reason: it is signed by this process, but a process with no password for the
// realm has nothing to have verified before signing.
func (g *guard) allow(realm Realm, r *http.Request) bool {
	if realm == RealmPublic {
		return true
	}
	if _, ok := g.digests[realm]; !ok {
		return false
	}
	return g.bearerMatches(realm, r) || g.sess.valid(realm, r)
}

// bearerMatches is the header credential on its own.
func (g *guard) bearerMatches(realm Realm, r *http.Request) bool {
	want, ok := g.digests[realm]
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(BearerToken(r.Header)))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// verify compares a submitted password against realm's, in constant time. It is
// what the unlock endpoint calls, and it is here rather than there so that the
// digests never leave this file.
func (g *guard) verify(realm Realm, password string) bool {
	want, ok := g.digests[realm]
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

// requireRealm enforces the guard, writing the failure response itself. It
// returns false when the caller should stop.
func (g *guard) requireRealm(w http.ResponseWriter, r *http.Request, realm Realm) bool {
	if g.allow(realm, r) {
		return true
	}
	// The realm is named in the challenge because there are two of them with
	// different passwords, and an agent that got the wrong one otherwise has no way
	// to tell which. The value is a constant from this package, never anything the
	// request supplied — a reflected header value here is a response-splitting
	// vector.
	w.Header().Set("WWW-Authenticate", `Bearer realm="`+string(realm)+`"`)
	if _, ok := g.digests[realm]; !ok {
		// Worth distinguishing in the log: one of these is a client with the wrong
		// password, the other is a controller that was deployed without one and
		// will refuse every request until it is redeployed.
		g.logf("auth: refusing %s %s — no token is configured for realm %q",
			r.Method, r.URL.Path, realm)
	} else {
		g.logf("auth: refusing %s %s — wrong or missing credential for realm %q",
			r.Method, r.URL.Path, realm)
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}
