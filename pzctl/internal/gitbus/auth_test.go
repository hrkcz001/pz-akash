package gitbus

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"strings"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
)

// testKey returns a fresh ed25519 pair: the PEM private key a deploy key would
// be, and the public half as a host key.
func testKey(t *testing.T) (pemBytes []byte, pub cryptossh.PublicKey) {
	t.Helper()
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := cryptossh.MarshalPrivateKey(privKey, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := cryptossh.NewPublicKey(pubKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block), sshPub
}

func knownHostsLine(host string, key cryptossh.PublicKey) string {
	return host + " " + strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(key)))
}

func TestHostKeyCallbackAcceptsOnlyPinnedKeys(t *testing.T) {
	_, pinned := testKey(t)
	_, impostor := testKey(t)

	cb, err := hostKeyCallback(knownHostsLine("github.com", pinned))
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}

	// go-git hands the callback a host:port pair, so the port must not defeat the
	// lookup.
	for _, host := range []string{"github.com", "github.com:22", "[github.com]:22"} {
		if err := cb(host, &net.TCPAddr{}, pinned); err != nil {
			t.Errorf("pinned key rejected for %q: %v", host, err)
		}
	}
	// The next thing we would do after accepting is present a repository write
	// key, so both of these must fail.
	if err := cb("github.com:22", &net.TCPAddr{}, impostor); err == nil {
		t.Error("a different key was accepted for a pinned host")
	}
	if err := cb("gitlab.com:22", &net.TCPAddr{}, pinned); err == nil {
		t.Error("an unpinned host was accepted")
	}
}

func TestHostKeyCallbackReadsEveryPinnedLine(t *testing.T) {
	// The real config pins three keys for github.com (ed25519, ecdsa, rsa),
	// because the server picks the algorithm. Stopping after the first would work
	// until GitHub's negotiation changed.
	_, a := testKey(t)
	_, b := testKey(t)
	_, c := testKey(t)
	content := strings.Join([]string{
		"# a comment, as api.github.com/meta output is often annotated",
		knownHostsLine("github.com", a),
		"",
		knownHostsLine("github.com", b),
		knownHostsLine("other.example", c),
	}, "\n") + "\n"

	cb, err := hostKeyCallback(content)
	if err != nil {
		t.Fatalf("hostKeyCallback: %v", err)
	}
	for i, key := range []cryptossh.PublicKey{a, b} {
		if err := cb("github.com:22", &net.TCPAddr{}, key); err != nil {
			t.Errorf("pinned key %d rejected: %v", i, err)
		}
	}
	if err := cb("github.com:22", &net.TCPAddr{}, c); err == nil {
		t.Error("a key pinned for another host was accepted for github.com")
	}
}

func TestHostKeyCallbackRejectsUnusableConfig(t *testing.T) {
	_, key := testKey(t)
	for _, tc := range []struct{ name, content string }{
		{"empty", ""},
		{"whitespace", "  \n\t\n"},
		{"comments only", "# nothing but a comment\n"},
		// Hashed entries are what `ssh-keyscan -H` writes. We do not implement the
		// HMAC lookup, and silently matching nothing would mean silently trusting
		// nothing — a confusing failure at connect time instead of a clear one at
		// startup.
		{"hashed", "|1|" + strings.TrimSpace(strings.SplitN(knownHostsLine("x", key), " ", 2)[1])},
	} {
		if _, err := hostKeyCallback(tc.content); err == nil {
			t.Errorf("hostKeyCallback(%s) accepted unusable content", tc.name)
		}
	}
}

func TestBuildAuthFailsClosedWithoutPinnedKeys(t *testing.T) {
	pemBytes, hostKey := testKey(t)

	// The dangerous case: a real SSH deploy key, no pinned host keys, and no
	// explicit opt-out. v1's answer here was StrictHostKeyChecking=no. Ours is to
	// refuse to build the auth at all, so the failure is at startup with a message
	// naming the config field.
	if _, err := buildAuth(Options{RepoURL: "git@github.com:o/r.git", DeployKeyPEM: pemBytes}); err == nil {
		t.Fatal("buildAuth produced credentials with no host-key verification")
	} else if !strings.Contains(err.Error(), "known_hosts") {
		t.Fatalf("error %q does not name the field to fix", err)
	}

	// With keys pinned, it builds and verifies.
	auth, err := buildAuth(Options{
		RepoURL:      "git@github.com:o/r.git",
		DeployKeyPEM: pemBytes,
		KnownHosts:   knownHostsLine("github.com", hostKey),
	})
	if err != nil {
		t.Fatalf("buildAuth with pinned keys: %v", err)
	}
	if auth == nil {
		t.Fatal("buildAuth returned no credential")
	}

	// A local test remote needs no credential at all, and must not be forced to
	// invent pinned host keys for a path on disk.
	if auth, err := buildAuth(Options{RepoURL: t.TempDir()}); err != nil || auth != nil {
		t.Fatalf("buildAuth for a path remote = %v, %v; want no credential and no error", auth, err)
	}

	// The explicit opt-out works, and is the only way to skip verification.
	// config.Validate refuses to let it coexist with an SSH repo_url.
	if _, err := buildAuth(Options{
		RepoURL:             "git@github.com:o/r.git",
		DeployKeyPEM:        pemBytes,
		AllowUnverifiedHost: true,
	}); err != nil {
		t.Fatalf("buildAuth with AllowUnverifiedHost: %v", err)
	}

	// A malformed key is an error, not a nil credential that fails later with a
	// permission-denied nobody can explain.
	if _, err := buildAuth(Options{
		RepoURL:      "git@github.com:o/r.git",
		DeployKeyPEM: []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnope\n-----END OPENSSH PRIVATE KEY-----\n"),
		KnownHosts:   knownHostsLine("github.com", hostKey),
	}); err == nil {
		t.Fatal("buildAuth accepted a malformed deploy key")
	}
}

func TestOpenRejectsIncompleteOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no repo", Options{CacheDir: t.TempDir()}},
		{"no cache dir", Options{RepoURL: "git@github.com:o/r.git"}},
	} {
		if _, err := Open(tc.opts); err == nil {
			t.Errorf("Open(%s) accepted incomplete options", tc.name)
		}
	}
}

func TestStripPort(t *testing.T) {
	for in, want := range map[string]string{
		"github.com":       "github.com",
		"github.com:22":    "github.com",
		"[github.com]:443": "github.com",
		"10.0.0.1:22":      "10.0.0.1",
		"[::1]:22":         "::1",
	} {
		if got := stripPort(in); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", in, got, want)
		}
	}
}
