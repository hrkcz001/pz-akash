package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

func TestPublicPackagesNeedNoCredential(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := makeZip(t, filepath.Join(h.packages, "client.zip"), []zipEntry{
		{name: "Client/options.ini", body: "Resolution=1920x1080"},
	})
	makeZip(t, filepath.Join(h.packages, "common.zip"), []zipEntry{{name: "mods/a.pak", body: "x"}})

	for _, path := range []string{PathClientZip, PathCommonZip} {
		resp := h.do(http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: this is what players download", path, resp.StatusCode)
		}
	}

	// ServeContent's Content-Length and Range support are the reason the public
	// packages take the plain path: a resumable transfer over a bad connection is
	// the difference between a player joining and a player giving up.
	resp := h.do(http.MethodGet, PathClientZip, "", nil, [2]string{"Range", "bytes=0-9"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("a Range request on client.zip = %d, want 206", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body[:10]) {
		t.Fatalf("Range body = %q, want the first ten bytes", got)
	}
}

func TestServerZipRequiresItsRealmAndSubstitutes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if err := writeFixture(filepath.Join(h.packages, "server.zip"), serverZipFixture(t)); err != nil {
		t.Fatal(err)
	}

	if resp := h.do(http.MethodGet, PathServerZip, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /server.zip = %d, want 401", resp.StatusCode)
	}
	// The backups token must not open server.zip. Two realms with two passwords is
	// the whole point: an operator who has the backup credential to fetch archives
	// has not thereby been given the game's passwords.
	if resp := h.do(http.MethodGet, PathServerZip, testSecs.BackupsPassword, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /server.zip with the backups token = %d, want 401", resp.StatusCode)
	}

	resp := h.do(http.MethodGet, PathServerZip, testSecs.ServerFilesPassword, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /server.zip = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	ini := readZip(t, got)["Server/vsrania.ini"]
	if !strings.Contains(ini, testSecs.RCONPassword) {
		t.Fatalf("the served .ini has no real RCON password:\n%s", ini)
	}
	// No ETag and no Content-Length: the bytes are generated per request, so a
	// cached copy or a Range against them would name an offset in a body that does
	// not exist yet.
	if resp.Header.Get("Etag") != "" {
		t.Fatalf("server.zip carries an ETag (%q) for a body generated per request",
			resp.Header.Get("Etag"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}
}

// A realm with no configured token denies everything. The opposite default —
// no password means no check — is how v1's storage server ended up serving
// /server.zip unauthenticated whenever the env var failed to reach the
// container, a state nobody could observe from outside because a working
// download looks identical either way.
func TestARealmWithNoTokenRefusesEverything(t *testing.T) {
	h := newHarness(t, harnessOptions{noSecrets: true})
	if err := writeFixture(filepath.Join(h.packages, "server.zip"), serverZipFixture(t)); err != nil {
		t.Fatal(err)
	}

	for _, token := range []string{"", "guess", testSecs.ServerFilesPassword} {
		resp := h.do(http.MethodGet, PathServerZip, token, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET /server.zip with token %q on a controller with no secrets = %d, want 401",
				token, resp.StatusCode)
		}
	}
	// And the log has to distinguish the two causes, because one is a client with
	// the wrong password and the other is a controller that needs redeploying.
	found := false
	for _, l := range h.logs {
		if strings.Contains(l, "no token is configured") {
			found = true
		}
	}
	if !found {
		t.Fatalf("nothing in the log says the realm has no token; logs = %v", h.logs)
	}

	// The public packages still work. A missing server-files password must not take
	// the client download down with it.
	makeZip(t, filepath.Join(h.packages, "client.zip"), []zipEntry{{name: "a", body: "x"}})
	if resp := h.do(http.MethodGet, PathClientZip, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /client.zip = %d, want 200", resp.StatusCode)
	}
}

func TestAMalformedAuthorizationHeaderIsNotAnEmptyPassword(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.write("backup_20260819_010000.zip", 8, testNow)
	if _, err := h.store.Rescan(); err != nil {
		t.Fatal(err)
	}

	for _, header := range []string{
		"Bearer",
		"Bearer ",
		"Basic " + testSecs.BackupsPassword,
		testSecs.BackupsPassword,
		"Bearer " + testSecs.BackupsPassword + "x",
	} {
		resp := h.do(http.MethodGet, BackupPath("backup_20260819_010000.zip"), "", nil,
			[2]string{"Authorization", header})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("Authorization: %q = %d, want 401", header, resp.StatusCode)
		}
	}
	// What must be accepted: the scheme is case-insensitive and RFC 9110 allows
	// 1*SP between it and the token, so trimming is conformance rather than
	// leniency.
	for _, header := range []string{
		"bearer " + testSecs.BackupsPassword,
		"BEARER " + testSecs.BackupsPassword,
		"Bearer  " + testSecs.BackupsPassword,
	} {
		resp := h.do(http.MethodGet, BackupPath("backup_20260819_010000.zip"), "", nil,
			[2]string{"Authorization", header})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Authorization: %q = %d, want 200", header, resp.StatusCode)
		}
	}
}

func TestUnauthorizedNamesTheRealmInTheChallenge(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	if err := writeFixture(filepath.Join(h.packages, "server.zip"), serverZipFixture(t)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Realm{
		PathServerZip:                            RealmServerFiles,
		BackupPath("backup_20260819_010000.zip"): RealmBackups,
	}
	for path, realm := range cases {
		resp := h.do(http.MethodGet, path, "wrong", nil)
		// There are two realms with different passwords, and an agent that got the
		// wrong one otherwise has no way to tell which it needs.
		want := `Bearer realm="` + string(realm) + `"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Fatalf("%s: WWW-Authenticate = %q, want %q", path, got, want)
		}
	}
}

func TestBackupPathsThatAreNotOneFileAre404(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	// Rejecting the shape before any path join means no handler ever joins an
	// attacker-controlled string onto a directory. These have to fail on the shape,
	// not on the filesystem happening not to hold the target.
	for _, path := range []string{
		"/backups/",
		"/backups/../api.go",
		"/backups/sub/dir.zip",
		"/backups/.",
	} {
		resp := h.do(http.MethodGet, path, testSecs.BackupsPassword, nil)
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
			t.Fatalf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestBackupDownloadCarriesTheDigestAndMarksTheCopy(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("w"), 1024)
	name := "backup_20260819_013623.zip"
	if _, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body)); err != nil {
		t.Fatal(err)
	}

	resp := h.do(http.MethodGet, BackupPath(name), testSecs.BackupsPassword, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d, want 200", resp.StatusCode)
	}
	// The digest travels on the download so an operator can verify the copy on their
	// laptop against the same value the agent verified against. It is the one header
	// that makes "I have a backup" checkable rather than assumed.
	if got := resp.Header.Get(HeaderSHA256); got != sha256Hex(body) {
		t.Fatalf("%s = %q, want %s", HeaderSHA256, got, sha256Hex(body))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), name) {
		t.Fatalf("Content-Disposition = %q, want it to name the file",
			resp.Header.Get("Content-Disposition"))
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("body is %d bytes, want %d", len(got), len(body))
	}
	if h.store.Index().Find(name).DownloadedAt.Zero() {
		t.Fatal("a completed download was not recorded; the dashboard would keep " +
			"warning that this archive exists only here")
	}
}

func TestARangeRequestDoesNotCountAsACopy(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("w"), 1024)
	name := "backup_20260819_013623.zip"
	if _, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body)); err != nil {
		t.Fatal(err)
	}

	// A partial fetch is what a resuming client does. Counting it would mark an
	// archive nobody has whole — and the stamp is the only thing standing between
	// "a copy exists off this disk" and a prune deleting the last one.
	resp := h.do(http.MethodGet, BackupPath(name), testSecs.BackupsPassword, nil,
		[2]string{"Range", "bytes=0-9"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range = %d, want 206", resp.StatusCode)
	}
	if !h.store.Index().Find(name).DownloadedAt.Zero() {
		t.Fatal("a Range request was recorded as a full download")
	}

	// HEAD likewise: it transfers nothing.
	if resp := h.do(http.MethodHead, BackupPath(name), testSecs.BackupsPassword, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD = %d, want 200", resp.StatusCode)
	}
	if !h.store.Index().Find(name).DownloadedAt.Zero() {
		t.Fatal("a HEAD was recorded as a download")
	}
}

func TestMissingBackupIs404(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	resp := h.do(http.MethodGet, BackupPath("backup_20260819_013623.zip"), testSecs.BackupsPassword, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a missing backup = %d, want 404", resp.StatusCode)
	}
}

func TestUploadRoundTripCarriesTheRequestID(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 4096)
	name := "backup_20260819_013623.zip"

	resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body),
		[2]string{HeaderSHA256, sha256Hex(body)},
		[2]string{HeaderRequestID, "req-halt-7"},
		[2]string{HeaderPhase, "halting"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}

	var out UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The agent compares the echoed digest with what it sent, so a controller that
	// silently rewrote the file is caught by the uploader rather than by a future
	// restore.
	if out.Name != name || out.Size != int64(len(body)) || out.SHA256 != sha256Hex(body) {
		t.Fatalf("UploadResult = %+v, want the name, %d bytes and the body digest", out, len(body))
	}

	// Bug 4: v1's controller could not tell the backup it asked for from one that
	// happened to arrive, so a halt waited for a report it had already been sent.
	// The request id has to reach the log, which is where the FSM's correlation is
	// observable.
	found := false
	for _, l := range h.logs {
		if strings.Contains(l, "req-halt-7") && strings.Contains(l, "halting") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the request id and phase did not reach the log; logs = %v", h.logs)
	}
	indexMatchesDisk(t, h)
}

func TestUploadOfAnExistingNameAnswers200(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 128)
	name := "backup_20260819_013623.zip"
	hdr := [2]string{HeaderSHA256, sha256Hex(body)}

	if resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body), hdr); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first PUT = %d, want 201", resp.StatusCode)
	}
	// A retry after a response the sender never saw. It must succeed — PUT is
	// idempotent — and the 200 is what lets a caller tell a retry that landed twice
	// from two archives it thought it had.
	resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body), hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200", resp.StatusCode)
	}
	if got := h.store.Index().Names(); len(got) != 1 {
		t.Fatalf("index = %v, want one entry", got)
	}
}

func TestUploadFailuresGetTheStatusThatTellsTheAgentWhatToDo(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 4096)
	name := "backup_20260819_013623.zip"

	t.Run("digest mismatch is 422", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body),
			[2]string{HeaderSHA256, sha256Hex([]byte("other"))})
		// Not 400: the request was well-formed and the sender's intent was clear. 422
		// says "I understood and the content is wrong", which tells the agent to retry
		// the transfer rather than stop and report a bad request.
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("PUT = %d, want 422", resp.StatusCode)
		}
		if got := h.onDisk(); len(got) != 0 {
			t.Fatalf("the directory holds %v after a 422", got)
		}
	})

	t.Run("over the limit is 413", func(t *testing.T) {
		h := newHarness(t, harnessOptions{maxUpload: 1024})
		resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body))
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("PUT = %d, want 413", resp.StatusCode)
		}
	})

	t.Run("no space is 507", func(t *testing.T) {
		h := newHarness(t, harnessOptions{minFree: 2 << 30})
		h.free = 1 << 20
		resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body))
		if resp.StatusCode != http.StatusInsufficientStorage {
			t.Fatalf("PUT = %d, want 507", resp.StatusCode)
		}
		// The response body must not quote the numbers back: a 507 that names the
		// free space is telling a caller the size of the disk.
		msg, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(msg), "bytes available") {
			t.Fatalf("the 507 body leaks the disk figures: %q", msg)
		}
	})

	t.Run("no credential is 401 and writes nothing", func(t *testing.T) {
		h := newHarness(t, harnessOptions{})
		resp := h.do(http.MethodPut, BackupPath(name), "", bytes.NewReader(body))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("PUT = %d, want 401", resp.StatusCode)
		}
		if got := h.onDisk(); len(got) != 0 {
			t.Fatalf("an unauthenticated PUT left %v on the disk", got)
		}
	})
}

func TestUploadWithoutADigestIsAllowedButLogged(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 128)
	name := "backup_20260819_013623.zip"

	// An operator uploading an archive from their laptop with curl has no easy way
	// to compute a digest first, and refusing them would break the one restore path
	// that exists when the disk is gone.
	resp := h.do(http.MethodPut, BackupPath(name), testSecs.BackupsPassword, bytes.NewReader(body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT with no digest = %d, want 201", resp.StatusCode)
	}
	found := false
	for _, l := range h.logs {
		if strings.Contains(l, "without a digest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("an undigested upload was not logged; logs = %v", h.logs)
	}
}

func TestBackupsIndexIsPublicAndUncached(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	body := bytes.Repeat([]byte("z"), 64)
	name := "backup_20260819_013623.zip"
	if _, err := h.store.Put(name, bytes.NewReader(body), int64(len(body)), sha256Hex(body)); err != nil {
		t.Fatal(err)
	}

	// Public, like the two open packages: names, sizes and digests are not secrets,
	// and the dashboard reads this from a browser holding no bearer token.
	resp := h.do(http.MethodGet, PathBackupsIndex, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", PathBackupsIndex, resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store: a cached index is how an agent "+
			"verifies a restore against the digest of an archive that has been replaced", got)
	}

	var idx state.Backups
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Items) != 1 || idx.Items[0].Name != name || idx.Items[0].SHA256 != sha256Hex(body) {
		t.Fatalf("index = %+v, want the one archive with its digest", idx.Items)
	}
	// Listing an archive is not permission to fetch it.
	if r := h.do(http.MethodGet, BackupPath(name), "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous download of a publicly listed archive = %d, want 401", r.StatusCode)
	}
}

func TestWrongMethodsAnswer405WithAllow(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	cases := map[string]string{
		PathHealth:       "GET, HEAD",
		PathBackupsIndex: "GET, HEAD",
		PathClientZip:    "GET, HEAD",
	}
	for path, allow := range cases {
		resp := h.do(http.MethodDelete, path, "", nil)
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE %s = %d, want 405", path, resp.StatusCode)
		}
		// The Allow header is what lets a client tell "wrong verb" from "no such
		// path", which is the difference between a bug in the caller and a bug in the
		// router.
		if got := resp.Header.Get("Allow"); got != allow {
			t.Fatalf("DELETE %s: Allow = %q, want %q", path, got, allow)
		}
	}
	resp := h.do(http.MethodPost, BackupPath("backup_20260819_013623.zip"), testSecs.BackupsPassword, strings.NewReader("x"))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST a backup = %d, want 405 (v1 used POST /upload; v2 does not)", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, PUT" {
		t.Fatalf("Allow = %q, want GET, HEAD, PUT", got)
	}
}

func TestHealthIsOpenAndSaysNothingElse(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	resp := h.do(http.MethodGet, PathHealth, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: the Akash probe carries no credential",
			PathHealth, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("health body = %q, want \"ok\"", body)
	}
}

func TestExtraHandlerGetsWhatThisPackageDoesNotClaim(t *testing.T) {
	// Step 7's dashboard mounts here. The assertion that matters is that it does
	// not shadow the file paths: a dashboard route swallowing /server.zip would
	// leave the agent unable to boot with a page that renders fine.
	h := newHarness(t, harnessOptions{})
	makeZip(t, filepath.Join(h.packages, "client.zip"), []zipEntry{{name: "a", body: "x"}})

	srv, err := NewServer(ServerOptions{
		Store: h.store,
		Cfg:   h.srvConfig(),
		Extra: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "dashboard "+r.URL.Path)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := srv.Handler()

	for path, want := range map[string]string{
		"/":           "dashboard /",
		"/ru":         "dashboard /ru",
		PathClientZip: "",
	} {
		rr := serve(mux, http.MethodGet, path)
		if want == "" {
			if strings.HasPrefix(rr.body, "dashboard") {
				t.Fatalf("%s was served by the extra handler", path)
			}
			continue
		}
		if rr.body != want {
			t.Fatalf("GET %s = %q, want %q", path, rr.body, want)
		}
	}
}
