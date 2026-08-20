package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// The harness for the step 4 gate. It assembles the three things the agent talks
// to — a real git remote, an HTTP controller, and a PZ process — with only the
// last two faked, and neither of them faked at the boundary the agent cares
// about: the git bus is the real gitbus, the HTTP client is the real Client, and
// the PZ process is a real child process with real pipes.
//
// That combination is the point. Every one of the four v1 bugs lived in the gap
// between two components that were each individually plausible, so a test that
// stubs the seam between them would reproduce none of them.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// recordedUpload is one PUT the fake controller received.
type recordedUpload struct {
	Name      string
	RequestID string
	Phase     string
	SHA256    string
	Body      []byte
}

type harness struct {
	t      *testing.T
	dir    string
	cfg    *config.Config
	remote string
	touch  string

	// The controller's side of the bus. The test plays the controller by mutating
	// doc/idx and publishing, which is exactly what internal/fsm will do in step 6.
	ctrlBus *gitbus.ControllerBus
	doc     *state.Controller
	idx     *state.Backups

	srv *httptest.Server

	mu      sync.Mutex
	uploads []recordedUpload
	served  map[string][]byte // backups the controller can serve for a restore
	denied  map[string]bool   // paths to answer 503 once, to exercise the retry
	lines   []string          // the agent's own log, for assertions about order

	agent  *Agent
	cancel context.CancelFunc
	runErr chan error
}

// tempDir is t.TempDir with a cleanup that cannot fail the test.
//
// The bare remote lives in here, and on Windows a git-upload-pack subprocess can
// still hold a handle to it when the test ends. t.TempDir's own cleanup calls
// t.Errorf in that case, which fails a test whose every assertion passed. Retry
// for a few seconds, then leave the directory behind and say so: the agent is
// what is under test here, not the platform's file locking.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pzagent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("left %s behind: a subprocess still holds a handle to it", dir)
	})
	return dir
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireGit(t)
	t.Setenv(fakePZEnv, "1")

	dir := tempDir(t)
	h := &harness{
		t:      t,
		dir:    dir,
		remote: filepath.Join(dir, "remote.git"),
		touch:  filepath.Join(dir, "launches.txt"),
		served: map[string][]byte{},
		denied: map[string]bool{},
		runErr: make(chan error, 1),
	}
	runGit(t, "", "init", "--bare", "--initial-branch=main", h.remote)
	t.Setenv(fakeTouchEnv, h.touch)

	h.srv = httptest.NewServer(http.HandlerFunc(h.handle))
	t.Cleanup(h.srv.Close)

	h.cfg = h.buildConfig()
	h.seedGameDir()

	// The controller's document starts where a real one does after a successful
	// deploy: running, with its own URLs published so the agent can find it.
	h.doc = state.NewController(h.cfg.Location())
	h.doc.Intent = state.IntentRunning
	h.doc.Status = state.StatusBooting
	h.doc.URLs = state.URLs{Raw: h.srv.URL}
	h.idx = state.NewBackups()

	h.ctrlBus = h.openControllerBus()
	h.publish("seed")
	return h
}

func (h *harness) buildConfig() *config.Config {
	c := config.Defaults()
	c.Identity.ServerName = "vsrania"
	c.Identity.Timezone = "Europe/Prague"
	c.Game.Map = "Muldraugh, KY"
	c.Game.PublicName = "vsrania"
	c.Game.MaxPlayers = 24

	c.Git.RepoURL = h.remote
	c.Git.CacheDir = filepath.Join(h.dir, "controller-mirror.git")
	c.Git.UserName = "pzctl"
	c.Git.UserEmail = "pzctl@example.invalid"
	// No floor: the test asserts on published documents, and a five-second floor
	// would make every assertion a five-second wait.
	c.Git.MinPushInterval = 0
	// Short on purpose. A git subprocess that wedges — which the local transport
	// does under this much concurrency on Windows — must surface as a failed
	// reconcile the loop recovers from, not as a hung test.
	c.Git.NetTimeout = config.Duration(10 * time.Second)

	home := filepath.Join(h.dir, "home")
	c.Agent.Paths.Home = home
	c.Agent.Paths.GameDir = filepath.Join(home, "pz-server")
	c.Agent.Paths.DataDir = filepath.Join(home, "Zomboid")
	c.Agent.Paths.LowercaseLink = filepath.Join(home, "zomboid")
	if runtime.GOOS == "windows" {
		// ~/zomboid and ~/Zomboid are one directory on a case-insensitive
		// filesystem, so the agent correctly refuses to replace a real directory
		// with the symlink. The link only exists for ext4, where the two names are
		// distinct; there is nothing to test for it here.
		c.Agent.Paths.LowercaseLink = ""
	}
	c.Agent.Paths.RepoCache = filepath.Join(h.dir, "agent-mirror.git")
	c.Agent.Paths.WorkDir = filepath.Join(home, "work")
	c.Agent.Paths.LogFile = filepath.Join(home, "server.log")

	c.Agent.Reconcile = config.Duration(150 * time.Millisecond)
	c.Agent.LivenessPush = config.Duration(2 * time.Second)
	c.Agent.PlayersPushMinInterval = 0
	c.Agent.RestoreDownloadRetries = 2
	c.Agent.RestoreDownloadTimeout = config.Duration(20 * time.Second)
	c.Agent.PZ = fakePZConfig()

	c.Server.Crash.MaxRestarts = 2
	c.Server.Crash.Backoff = config.Duration(100 * time.Millisecond)
	c.Backups.HaltTimeout = config.Duration(30 * time.Second)
	c.Backups.UploadMaxBytes = 1 << 30
	return c
}

// seedGameDir creates the parts of the image the agent expects to find: the game
// directory and the launcher's JSON. The launcher itself is this test binary,
// injected in start().
func (h *harness) seedGameDir() {
	writeFile(h.t, filepath.Join(h.cfg.Agent.Paths.GameDir, "ProjectZomboid64.json"),
		`{"mainClass":"zombie/network/GameServer","vmArgs":["-Xmx2048m","-Dzomboid.steam=1"]}`)
}

func (h *harness) secrets() *secrets.Set {
	return &secrets.Set{ServerFilesPassword: "sf-token", BackupsPassword: "bk-token"}
}

func (h *harness) openControllerBus() *gitbus.ControllerBus {
	h.t.Helper()
	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:    h.remote,
		CacheDir:   h.cfg.Git.CacheDir,
		UserName:   h.cfg.Git.UserName,
		UserEmail:  h.cfg.Git.UserEmail,
		Location:   h.cfg.Location(),
		NetTimeout: h.cfg.Git.NetTimeout.D(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	bl := h.cfg.Git.BranchLayout()
	bus, err := gitbus.NewControllerBus(repo, gitbus.Branches{
		Main: bl.Main, Controller: bl.Controller, Agent: bl.Agent, TriggersDir: bl.TriggersDir,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return bus
}

// publish writes the controller's document and index, the way the controller
// does. Everything the agent obeys arrives through here and nowhere else.
func (h *harness) publish(reason string) {
	h.t.Helper()
	if err := h.ctrlBus.Fetch(context.Background()); err != nil {
		h.t.Fatalf("controller fetch: %v", err)
	}
	if _, err := h.ctrlBus.Publish(context.Background(), h.doc, h.idx, reason); err != nil {
		h.t.Fatalf("controller publish: %v", err)
	}
}

// agentDoc reads the agent's document as the controller sees it. Deliberately not
// a peek at h.agent.doc: what the controller can read is the whole of the
// contract, and a field the agent updates but never publishes is invisible to it.
func (h *harness) agentDoc() *state.Agent {
	h.t.Helper()
	if err := h.ctrlBus.Fetch(context.Background()); err != nil {
		h.t.Fatalf("controller fetch: %v", err)
	}
	doc, _, err := h.ctrlBus.ReadAgent()
	if err != nil {
		h.t.Fatalf("read the agent document: %v", err)
	}
	return doc
}

// start builds the agent and runs it in the background.
func (h *harness) start() {
	h.t.Helper()

	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:    h.remote,
		CacheDir:   h.cfg.Agent.Paths.RepoCache,
		UserName:   h.cfg.Git.UserName,
		UserEmail:  h.cfg.Git.UserEmail,
		Location:   h.cfg.Location(),
		NetTimeout: h.cfg.Git.NetTimeout.D(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	bl := h.cfg.Git.BranchLayout()
	bus, err := gitbus.NewAgentBus(repo, gitbus.Branches{
		Main: bl.Main, Controller: bl.Controller, Agent: bl.Agent, TriggersDir: bl.TriggersDir,
	})
	if err != nil {
		h.t.Fatal(err)
	}

	a, err := New(Options{
		Config:  h.cfg,
		Secrets: h.secrets(),
		Bus:     bus,
		// Deliberately empty: the agent must discover the controller from the
		// state branch. v1 baked in CONTROLLER_URL=http://controller:8000, a value
		// that could not resolve from another provider's cluster.
		ControllerURL: "",
		Logf:          h.logf,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	// The launcher is this test binary; TestMain turns it into a PZ server when it
	// sees PZ_FAKE_PZ. Set directly, so the test does not depend on a file named
	// start-server.sh being executable on the host — findLauncher has its own test.
	self, err := os.Executable()
	if err != nil {
		h.t.Fatal(err)
	}
	a.launcher = self
	h.agent = a

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.runErr <- a.Run(ctx) }()
	h.t.Cleanup(h.stop)
}

// stop cancels Run and waits for it, so a leaked PZ process fails the test rather
// than outliving it.
func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	h.cancel = nil
	select {
	case err := <-h.runErr:
		if err != nil {
			h.t.Errorf("Run returned %v; the agent must only ever return nil, on cancellation", err)
		}
	case <-time.After(30 * time.Second):
		// The dump is worth the noise: "Run did not return" is exactly the class of
		// bug this package exists to prevent, and the stack says which wait it was.
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		h.t.Errorf("Run did not return within 30s of cancellation\n%s", buf[:n])
	}
}

// launches is how many times a PZ process has been started.
func (h *harness) launches() int {
	body, err := os.ReadFile(h.touch)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(body)))
}

func (h *harness) savesDir() string { return filepath.Join(h.cfg.Agent.Paths.DataDir, "Saves") }

// --- the fake controller ---

func (h *harness) handle(w http.ResponseWriter, r *http.Request) {
	token := httpapi.BearerToken(r.Header)

	h.mu.Lock()
	if h.denied[r.URL.Path] {
		delete(h.denied, r.URL.Path)
		h.mu.Unlock()
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	h.mu.Unlock()

	switch {
	case r.URL.Path == httpapi.PathHealth:
		w.Write([]byte("ok"))

	case r.URL.Path == httpapi.PathCommonZip:
		// Absent, which is legitimate: a server with no shared mods has no
		// common.zip, and boot must survive the 404 rather than treat it as fatal.
		http.NotFound(w, r)

	case r.URL.Path == httpapi.PathServerZip:
		if token != "sf-token" {
			http.Error(w, "forbidden", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Write(h.serverZip())

	case strings.HasPrefix(r.URL.Path, httpapi.PathBackupsDir):
		name, ok := httpapi.BackupName(r.URL.Path)
		if !ok {
			http.Error(w, "bad backup name", http.StatusBadRequest)
			return
		}
		if token != "bk-token" {
			http.Error(w, "forbidden", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.mu.Lock()
			body, ok := h.served[name]
			h.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/zip")
			w.Write(body)
		case http.MethodPut:
			h.receive(w, r, name)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	default:
		http.NotFound(w, r)
	}
}

// receive stores an uploaded backup and answers with the digest it computed, the
// way the controller's own handler will in step 6.
func (h *harness) receive(w http.ResponseWriter, r *http.Request, name string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	up := recordedUpload{
		Name:      name,
		RequestID: r.Header.Get(httpapi.HeaderRequestID),
		Phase:     r.Header.Get(httpapi.HeaderPhase),
		SHA256:    r.Header.Get(httpapi.HeaderSHA256),
		Body:      body,
	}
	h.mu.Lock()
	h.uploads = append(h.uploads, up)
	h.served[name] = body
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(httpapi.UploadResult{
		Name: name, Size: int64(len(body)), SHA256: up.SHA256,
	})
}

// serverZip is the package the controller serves: the .ini with the passwords
// already substituted. The agent must patch the keys config.yaml owns and leave
// the passwords alone.
func (h *harness) serverZip() []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("Server/" + h.cfg.Identity.ServerName + ".ini")
	if err != nil {
		h.t.Fatal(err)
	}
	fmt.Fprint(f, strings.Join([]string{
		"# served by the controller",
		"Password=lobbyword",
		"RCONPassword=rconword",
		"AdminPassword=adminword",
		"MaxPlayers=16",
		"ZombieConfig=hordes",
	}, "\n")+"\n")
	if err := zw.Close(); err != nil {
		h.t.Fatal(err)
	}
	return buf.Bytes()
}

// serveBackup makes an archive available for a restore and indexes it. digest may
// be overridden to simulate an archive whose bytes do not match the index.
func (h *harness) serveBackup(name string, contents map[string]string, digest string) {
	h.t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for path, body := range contents {
		f, err := zw.Create(path)
		if err != nil {
			h.t.Fatal(err)
		}
		io.WriteString(f, body)
	}
	if err := zw.Close(); err != nil {
		h.t.Fatal(err)
	}
	body := buf.Bytes()

	h.mu.Lock()
	h.served[name] = body
	h.mu.Unlock()

	if digest == "" {
		tmp := filepath.Join(h.t.TempDir(), name)
		if err := os.WriteFile(tmp, body, 0o644); err != nil {
			h.t.Fatal(err)
		}
		var err error
		if digest, err = sha256File(tmp); err != nil {
			h.t.Fatal(err)
		}
	}
	h.idx.Upsert(state.Backup{
		Name:      name,
		Size:      int64(len(body)),
		SHA256:    digest,
		CreatedAt: state.Now(h.cfg.Location()),
	})
}

// uploadFor returns the upload answering a request ID, and how many uploads have
// arrived in total.
func (h *harness) uploadFor(requestID string) (recordedUpload, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found recordedUpload
	for _, u := range h.uploads {
		if u.RequestID == requestID {
			found = u
		}
	}
	return found, len(h.uploads)
}

// --- the agent's log ---

// logf records the agent's log and mirrors it into the test's output. The log is a
// legitimate observable here: a phase the agent enters and leaves between two git
// fetches is invisible in the published document but visible in this trail, and
// some of the properties worth pinning are about order rather than end state.
func (h *harness) logf(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
	h.t.Logf("agent: %s", line)
}

func (h *harness) sawLog(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// logOrder reports whether first appears before second, and whether both appear.
func (h *harness) logOrder(first, second string) (ok, both bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i, j := -1, -1
	for n, l := range h.lines {
		if i < 0 && strings.Contains(l, first) {
			i = n
		}
		if j < 0 && strings.Contains(l, second) {
			j = n
		}
	}
	return i >= 0 && j > i, i >= 0 && j >= 0
}

// --- waiting ---

// waitFor polls cond until it holds. Polling rather than signalling on purpose:
// the assertions are about what the controller can observe from the published
// document, and the controller polls too. The interval is a real git fetch, so it
// is deliberately not tight.
func (h *harness) waitFor(what string, timeout time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			doc := h.agentDoc()
			h.t.Fatalf("timed out after %v waiting for %s (phase=%s players=%d restarts=%d last_error=%q)",
				timeout, what, doc.Phase, doc.PlayersCount, doc.Restarts, doc.LastError)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// waitPhase waits for a published phase and returns the document that carried it.
func (h *harness) waitPhase(p state.Phase, timeout time.Duration) *state.Agent {
	h.t.Helper()
	var doc *state.Agent
	h.waitFor("phase "+string(p), timeout, func() bool {
		doc = h.agentDoc()
		return doc.Phase == p
	})
	return doc
}

// zipNames lists the entries of an archive held in memory.
func zipNames(t *testing.T, body []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	return names
}

// zipEntry returns one entry's contents.
func zipEntry(t *testing.T, body []byte, name string) (string, bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b), true
	}
	return "", false
}

// fileExists is used to assert a negative — that the game was never launched.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
