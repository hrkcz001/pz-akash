package httpapi

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// The fixtures below deliberately use a fixed clock and a fixed location. Every
// timestamp this package writes comes from identity.timezone, never from the
// host, and a test that passed only because the runner happened to be in Prague
// would be testing nothing.
var (
	testLoc  = mustLoad("Europe/Prague")
	testNow  = time.Date(2026, 8, 19, 1, 36, 23, 0, testLoc)
	testSecs = &secrets.Set{
		ServerFilesPassword: "server-files-token",
		BackupsPassword:     "backups-token",
		RCONPassword:        "rcon-hunter2",
		AdminPassword:       "admin-hunter2",
		JoinPassword:        "join-hunter2",
	}
)

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// clock is a hand-cranked time source. Nothing in this package sleeps, so a
// test that needs an hour to pass advances it instead of waiting.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// harness is a Store plus the Server in front of it, wired the way the
// controller wires them.
type harness struct {
	t        *testing.T
	store    *Store
	srv      *Server
	http     *httptest.Server
	dir      string
	packages string

	cfg *config.Config
	clk *clock

	// changes counts onChange calls, so a test can assert that a mutation
	// published a fresh index rather than only writing a file.
	changes []*state.Backups

	// logs is every line the components emitted, for the assertions about what an
	// operator would be able to see afterwards.
	logs []string

	// free is what the fake disk probe reports. Tests that care set it.
	free   int64
	freeOK bool
}

type harnessOptions struct {
	maxUpload int64
	minFree   int64
	// noSecrets builds the server with a nil secrets.Set — the shape a controller
	// takes when its env vars did not arrive.
	noSecrets bool
	// substitutePatterns overrides the default Server/*.ini.
	substitutePatterns []string
	substituteMax      int64
	uploadTimeout      time.Duration

	// sessionTTL and the two unlock limits populate config.Dashboard, which is
	// what turns on the cookie half of the guard. Zero leaves it off, and that is
	// the default: a controller with no dashboard has no unlock endpoint, and every
	// other test in this package authenticates with a bearer token.
	sessionTTL     time.Duration
	unlockAttempts int
	unlockWindow   time.Duration

	// torrentFile is dashboard.torrent_file. Empty is the default and means the
	// route does not exist at all.
	torrentFile string
}

func newHarness(t *testing.T, o harnessOptions) *harness {
	t.Helper()

	root := t.TempDir()
	h := &harness{
		t:        t,
		dir:      filepath.Join(root, "backups"),
		packages: filepath.Join(root, "packages"),
		clk:      &clock{t: testNow},
		free:     512 << 30,
		freeOK:   true,
	}
	if err := os.MkdirAll(h.packages, 0o755); err != nil {
		t.Fatal(err)
	}

	logf := func(format string, args ...any) {
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
	}

	store, err := NewStore(StoreOptions{
		Dir:       h.dir,
		Loc:       testLoc,
		MaxUpload: o.maxUpload,
		MinFree:   o.minFree,
		Logf:      logf,
		Now:       h.clk.now,
		OnChange:  func(idx *state.Backups) { h.changes = append(h.changes, idx) },
		freeBytes: func(string) (int64, bool) { return h.free, h.freeOK },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.store = store

	patterns := o.substitutePatterns
	if patterns == nil {
		patterns = []string{"Server/*.ini"}
	}
	max := o.substituteMax
	if max == 0 {
		max = 4 << 20
	}

	cfg := &config.Config{}
	cfg.Controller.Storage = config.Storage{
		PackagesDir:        h.packages,
		SubstituteEntries:  patterns,
		SubstituteMaxBytes: max,
		MinFreeBytes:       o.minFree,
		ReadHeaderTimeout:  config.Duration(30 * time.Second),
		UploadTimeout:      config.Duration(o.uploadTimeout),
		IdleTimeout:        config.Duration(120 * time.Second),
		ShutdownGrace:      config.Duration(30 * time.Second),
	}
	cfg.Dashboard = config.Dashboard{
		SessionTTL:     config.Duration(o.sessionTTL),
		UnlockAttempts: o.unlockAttempts,
		UnlockWindow:   config.Duration(o.unlockWindow),
		TorrentFile:    o.torrentFile,
	}
	h.cfg = cfg

	sec := testSecs
	if o.noSecrets {
		sec = nil
	}
	srv, err := NewServer(ServerOptions{
		Store:   store,
		Cfg:     cfg,
		Secrets: sec,
		Logf:    logf,
		Now:     h.clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.http = httptest.NewServer(srv.Handler())
	t.Cleanup(h.http.Close)
	return h
}

// do issues a request against the test server. token is attached as a bearer
// credential; "" sends no Authorization header at all, which is the shape an
// anonymous caller has.
func (h *harness) do(method, path, token string, body io.Reader, headers ...[2]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.http.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	SetAuth(req, token)
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// onDisk is the sorted list of backup files actually present, including the
// dotted temp files an interrupted upload would leave — which is the thing
// several tests are really asking about.
func (h *harness) onDisk() []string {
	h.t.Helper()
	ents, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// write puts a file straight into the backups directory, bypassing the Store —
// the way an operator with scp, or a v1 archive left behind by a migration,
// would.
func (h *harness) write(name string, size int, modTime time.Time) string {
	h.t.Helper()
	body := bytes.Repeat([]byte{byte(len(name))}, size)
	p := filepath.Join(h.dir, name)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		h.t.Fatal(err)
	}
	if !modTime.IsZero() {
		if err := os.Chtimes(p, modTime, modTime); err != nil {
			h.t.Fatal(err)
		}
	}
	return p
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// srvConfig is the config the harness built, for a test that needs a second
// Server over the same store.
func (h *harness) srvConfig() *config.Config { return h.cfg }

// writeFixture drops prepared bytes into packages_dir.
func writeFixture(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
}

// recorded is a response captured without a socket, for the routing assertions
// where a real transport would only add path cleaning.
type recorded struct {
	code   int
	header http.Header
	body   string
}

func serve(h http.Handler, method, path string) recorded {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
	return recorded{code: rr.Code, header: rr.Header(), body: rr.Body.String()}
}

// zipEntry is one member of a fixture archive.
type zipEntry struct {
	name string
	body string
	dir  bool
}

// makeZip writes a fixture archive to path and returns its bytes.
func makeZip(t *testing.T, path string, entries []zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		if e.dir {
			if _, err := zw.Create(e.name); err != nil {
				t.Fatal(err)
			}
			continue
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.Modified = testNow
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if path != "" {
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// readZip returns the archive's entries as name → body.
func readZip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

// rawBytes returns each entry's compressed bytes, which is what the raw-copy
// path is supposed to preserve exactly.
func rawBytes(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		r, err := f.OpenRaw()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		raw, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		out[f.Name] = raw
	}
	return out
}
