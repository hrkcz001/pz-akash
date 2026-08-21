package httpapi

import (
	"net/http"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

func TestGuardComparesFixedLengthDigests(t *testing.T) {
	g := newGuard(testSecs, nil)

	// The tokens are stored as digests so the comparison is fixed-length, which is
	// what lets the constant-time compare actually be constant time: comparing a
	// 4-character attempt against a 19-character password leaks the length through
	// the timing of the length check itself. Assert the shape rather than trying to
	// measure timing, which is not a thing a unit test can do honestly.
	for realm, want := range map[Realm]string{
		RealmServerFiles: testSecs.ServerFilesPassword,
		RealmBackups:     testSecs.BackupsPassword,
	} {
		d, ok := g.digests[realm]
		if !ok {
			t.Fatalf("no digest for realm %q", realm)
		}
		if len(d) != 32 {
			t.Fatalf("realm %q holds a %d-byte digest, want 32", realm, len(d))
		}
		if string(d[:]) == want {
			t.Fatalf("realm %q holds the password itself", realm)
		}
	}
}

func TestGuardAllow(t *testing.T) {
	g := newGuard(testSecs, nil)
	cases := []struct {
		name  string
		realm Realm
		token string
		want  bool
	}{
		{"public with nothing", RealmPublic, "", true},
		{"public ignores a wrong token", RealmPublic, "nonsense", true},
		{"right token", RealmBackups, testSecs.BackupsPassword, true},
		{"the other realm's token", RealmBackups, testSecs.ServerFilesPassword, false},
		{"empty", RealmBackups, "", false},
		{"a prefix of the password", RealmBackups, testSecs.BackupsPassword[:4], false},
		// A realm nobody registered is not a realm anyone can enter. Adding a path
		// with a typo in its realm name must close it, not open it.
		{"an unknown realm", Realm("typo"), testSecs.BackupsPassword, false},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		SetAuth(r, c.token)
		if got := g.allow(c.realm, r); got != c.want {
			t.Errorf("%s: allow(%q) = %v, want %v", c.name, c.realm, got, c.want)
		}
	}
}

func TestGuardWithNoSecretsClosesEveryRealmButPublic(t *testing.T) {
	for _, sec := range []*secrets.Set{nil, {}} {
		g := newGuard(sec, nil)
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		if !g.allow(RealmPublic, r) {
			t.Fatal("public was closed")
		}
		for _, realm := range []Realm{RealmServerFiles, RealmBackups} {
			// An empty password must not register a realm. Otherwise a request with no
			// Authorization header would hash to the same empty string and match —
			// which is v1's accidental open door, reintroduced.
			if _, ok := g.digests[realm]; ok {
				t.Fatalf("realm %q was registered from an empty password", realm)
			}
			if g.allow(realm, r) {
				t.Fatalf("realm %q was open with no password configured", realm)
			}
		}
	}
}
