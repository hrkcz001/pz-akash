package fsm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// These tests drive the machine against a real bare git remote, with a simulated
// agent that publishes through the real AgentBus on its own mirror.
//
// Nothing here stubs the bus. The bugs this rewrite exists to fix were all in the
// seam between the lifecycle and the transport — a trigger consumed twice, a state
// write mistaken for an operator request, a halt satisfied by somebody else's
// backup — and a fake bus is exactly the thing that cannot show them. The clock is
// fake, because timeouts must be testable without waiting them out; the git
// operations are not.
//
// Events are delivered by calling handle directly rather than by running the
// timers. That keeps every test deterministic and makes the assertions about
// ordering meaningful.

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// clock is a fake clock the tests advance by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness is a controller, a simulated agent, and the remote they share.
type harness struct {
	t      *testing.T
	cfg    *config.Config
	remote string
	work   string // an operator working copy, for pushing triggers
	br     gitbus.Branches
	m      *Machine
	dry    *DryRun
	clk    *clock
	agent  *gitbus.AgentBus
	adoc   *state.Agent
	logs   []string
	reqs   int
	mu     sync.Mutex
}

// newHarness seeds a remote with config.yaml on main and returns a machine
// positioned at offline.
func newHarness(t *testing.T, tune func(*config.Config)) *harness {
	t.Helper()
	requireGit(t)

	cfg := config.Defaults()
	cfg.Identity.Timezone = "Europe/Prague"
	cfg.Git.Branch = "main"
	cfg.Git.MinPushInterval = config.Duration(0) // no coalescing: assert per step
	cfg.Git.AllowUnverifiedHost = true
	if tune != nil {
		tune(cfg)
	}

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", remote)

	work := filepath.Join(root, "work")
	runGit(t, "", "init", "--initial-branch=main", work)
	if err := os.WriteFile(filepath.Join(work, "config.yaml"),
		[]byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.name=op", "-c", "user.email=op@example.invalid",
		"commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "push", "-u", "origin", "main")

	h := &harness{
		t: t, cfg: cfg, remote: remote, work: work,
		clk: &clock{t: time.Date(2026, 8, 19, 10, 0, 0, 0, cfg.Location())},
	}

	bl := cfg.Git.BranchLayout()
	br := gitbus.Branches{
		Main:        bl.Main,
		Controller:  bl.Controller,
		Agent:       bl.Agent,
		TriggersDir: bl.TriggersDir,
	}
	h.br = br

	// Two mirrors, because there are two processes. One shared Repo would make
	// every agent push look like the controller's own echo, which is precisely the
	// distinction the self-push filter has to get right.
	ctlRepo := h.openRepo(filepath.Join(root, "ctl.git"), "pzctl")
	bus, err := gitbus.NewControllerBus(ctlRepo, br)
	if err != nil {
		t.Fatal(err)
	}
	agentRepo := h.openRepo(filepath.Join(root, "agent.git"), "pz-agent")
	h.agent, err = gitbus.NewAgentBus(agentRepo, br)
	if err != nil {
		t.Fatal(err)
	}
	h.adoc = state.NewAgent(cfg.Location())

	h.dry = &DryRun{Cfg: cfg, Now: h.clk.now, Logf: h.logf}
	h.m, err = New(Deps{
		Cfg: cfg, Bus: bus, Akash: h.dry,
		Now: h.clk.now, NewID: h.nextID, Logf: h.logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.load(t.Context()); err != nil {
		t.Fatalf("load: %v", err)
	}
	return h
}

func (h *harness) openRepo(cache, who string) *gitbus.Repo {
	h.t.Helper()
	r, err := gitbus.Open(gitbus.Options{
		RepoURL:    h.remote,
		CacheDir:   cache,
		UserName:   who,
		UserEmail:  who + "@example.invalid",
		Location:   h.cfg.Location(),
		NetTimeout: h.cfg.Git.NetTimeout.D(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		h.t.Fatalf("open %s mirror: %v", who, err)
	}
	if err := r.Fetch(h.t.Context()); err != nil {
		h.t.Fatalf("fetch %s mirror: %v", who, err)
	}
	return r
}

// nextID is a deterministic request ID, so a failure message names a request the
// reader can find in the log.
func (h *harness) nextID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs++
	return fmt.Sprintf("req%d", h.reqs)
}

func (h *harness) logf(f string, a ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, fmt.Sprintf(f, a...))
}

// dumpLogs prints the machine's log when a test fails, which is the only readable
// account of a lifecycle that went wrong.
func (h *harness) dumpLogs() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.t.Log("machine log:\n  " + strings.Join(h.logs, "\n  "))
}

// logged reports whether any log line contains substr.
func (h *harness) logged(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// logCount is how many log lines contain substr, for the assertions that care that
// something happened a bounded number of times rather than merely happened.
func (h *harness) logCount(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// --- operator actions ---

// push writes files on main and pushes them, the way an operator does.
func (h *harness) push(files map[string]string) {
	h.t.Helper()
	runGit(h.t, h.work, "pull", "--rebase", "-q", "origin", "main")
	for path, body := range files {
		full := filepath.Join(h.work, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	runGit(h.t, h.work, "add", "-A")
	runGit(h.t, h.work, "-c", "user.name=op", "-c", "user.email=op@example.invalid",
		"commit", "-m", "operator push")
	runGit(h.t, h.work, "push", "-q", "origin", "main")
}

// trigger pushes one trigger file.
func (h *harness) trigger(name, body string) {
	h.t.Helper()
	h.push(map[string]string{h.br.TriggersDir + "/" + name: body})
}

// remove deletes paths from the operator branch, the way an operator clearing a
// pause file does.
func (h *harness) remove(paths ...string) {
	h.t.Helper()
	runGit(h.t, h.work, "pull", "--rebase", "-q", "origin", "main")
	for _, p := range paths {
		runGit(h.t, h.work, "rm", "-q", "--ignore-unmatch", filepath.FromSlash(p))
	}
	runGit(h.t, h.work, "-c", "user.name=op", "-c", "user.email=op@example.invalid",
		"commit", "-m", "operator remove")
	runGit(h.t, h.work, "push", "-q", "origin", "main")
}

func (h *harness) removePause() {
	h.t.Helper()
	h.remove(h.cfg.Backups.PauseFile)
}

// corruptControllerState force-pushes an unparseable controller.json, which is
// what a publish interrupted mid-write leaves behind. An orphan commit is used
// because that is how the real publisher writes the branch.
func (h *harness) corruptControllerState() {
	h.t.Helper()
	dir := h.t.TempDir()
	runGit(h.t, "", "init", "-q", "--initial-branch=state", dir)
	if err := os.WriteFile(filepath.Join(dir, gitbus.FileController),
		[]byte(`{"version":1,"status":"onl`), 0o644); err != nil {
		h.t.Fatal(err)
	}
	runGit(h.t, dir, "add", "-A")
	runGit(h.t, dir, "-c", "user.name=corrupt", "-c", "user.email=c@example.invalid",
		"commit", "-q", "-m", "truncated write")
	runGit(h.t, dir, "push", "-q", "--force", h.remote, "HEAD:refs/heads/"+h.br.Controller)
}

// triggersLeft lists what is still pending on the remote, read with git rather
// than through our own reader so a test cannot pass by being wrong twice.
func (h *harness) triggersLeft() []string {
	h.t.Helper()
	out, err := exec.Command("git", "-C", h.remote, "ls-tree", "--name-only",
		"main:"+h.br.TriggersDir).CombinedOutput()
	if err != nil {
		return nil // the directory is gone, which is what an empty set looks like
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	return names
}

// --- controller stepping ---

// poll delivers one poll event: fetch, read the agent, consume triggers, advance.
func (h *harness) poll() {
	h.t.Helper()
	h.m.handle(h.t.Context(), Poll("test", ""))
}

// tick delivers one housekeeping event.
func (h *harness) tick() {
	h.t.Helper()
	h.m.handle(h.t.Context(), Tick("test"))
}

// settle delivers queued events until no job is in flight, so a test can assert
// on the state after a deploy or a close has reported back.
func (h *harness) settle() {
	h.t.Helper()
	for i := 0; h.m.job != nil; i++ {
		if i > 20 {
			h.dumpLogs()
			h.t.Fatalf("job %q never reported back", h.m.job.what)
		}
		select {
		case ev := <-h.m.events:
			h.m.handle(h.t.Context(), ev)
		case <-time.After(10 * time.Second):
			h.dumpLogs()
			h.t.Fatalf("timed out waiting for %q", h.m.job.what)
		}
	}
}

// bringOnline runs the ordinary happy path up to online, so a test about
// something else does not have to restate it.
func (h *harness) bringOnline() {
	h.t.Helper()
	h.trigger("start", "")
	h.poll()
	h.settle()
	h.agentPhase(state.PhaseOnline)
	h.poll()
	h.wantStatus(state.StatusOnline)
}

// gate wraps a driver so a deploy blocks after it has created its lease, until
// the test releases it. A sleep would only make the race probable; this makes the
// ordering exact, which is what a test about a halt overtaking a deploy needs.
type gate struct {
	Akash
	open chan struct{}
}

func (g *gate) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	res, err := g.Akash.Deploy(ctx, req)
	if err != nil {
		return res, err
	}
	select {
	case <-g.open:
		return res, nil
	case <-ctx.Done():
		// The lease is still in res, which is the whole point: a cancelled deploy
		// that created something must hand it back.
		return res, ctx.Err()
	}
}

// holdDeploys makes every deploy park after creating its lease. The returned func
// releases them and must be called, or a blocked job outlives the test.
func (h *harness) holdDeploys() func() {
	h.t.Helper()
	g := &gate{Akash: h.m.akash, open: make(chan struct{})}
	h.m.akash = g
	var once sync.Once
	return func() { once.Do(func() { close(g.open) }) }
}

// --- agent simulation ---

// stamp is the simulated agent's instant, taken from the same fake clock the
// controller reads. A real agent stamps from its own clock; sharing one here is
// what makes a test about a timeout ("this report is older than halt_timeout")
// mean anything at all.
func (h *harness) stamp() state.Stamp {
	return state.At(h.clk.now()).In(h.cfg.Location())
}

// agentPhase publishes a phase change from the agent's own branch.
func (h *harness) agentPhase(p state.Phase) {
	h.t.Helper()
	h.adoc.SetPhase(p, h.stamp())
	h.publishAgent("phase " + string(p))
}

// agentBackup answers the outstanding request the way the real agent will: read
// the controller's branch for the request, then report against its ID.
func (h *harness) agentBackup(st state.BackupState, name string, size int64) {
	h.t.Helper()
	if err := h.agent.Fetch(h.t.Context()); err != nil {
		h.t.Fatalf("agent fetch: %v", err)
	}
	doc, _, _, err := h.agent.ReadController()
	if err != nil {
		h.t.Fatalf("agent read controller: %v", err)
	}
	if doc.BackupRequest == nil {
		h.dumpLogs()
		h.t.Fatal("agent found no outstanding backup request")
	}
	h.agentBackupID(doc.BackupRequest.ID, st, name, size)
}

// agentBackupID reports against an explicit ID, so a test can produce the stale
// report that v1 would have accepted.
func (h *harness) agentBackupID(id string, st state.BackupState, name string, size int64) {
	h.t.Helper()
	now := h.stamp()
	h.adoc.Backup = &state.BackupReport{
		RequestID: id, State: st, StartedAt: now,
	}
	switch st {
	case state.BackupDone:
		h.adoc.Backup.Name = name
		h.adoc.Backup.Size = size
		h.adoc.Backup.SHA256 = strings.Repeat("ab", 32)
		h.adoc.Backup.EndedAt = now
	case state.BackupFailed:
		h.adoc.Backup.Error = "simulated failure"
		h.adoc.Backup.EndedAt = now
	}
	h.publishAgent("backup " + id + " " + string(st))
}

func (h *harness) publishAgent(reason string) {
	h.t.Helper()
	h.adoc.Touch(h.stamp())
	if _, err := h.agent.Publish(h.t.Context(), h.adoc, reason); err != nil {
		h.t.Fatalf("agent publish (%s): %v", reason, err)
	}
}

// --- assertions ---

func (h *harness) wantStatus(want state.Status) {
	h.t.Helper()
	if got := h.m.doc.Status; got != want {
		h.dumpLogs()
		h.t.Fatalf("status = %s, want %s", got, want)
	}
}

func (h *harness) wantIntent(want state.Intent) {
	h.t.Helper()
	if got := h.m.doc.Intent; got != want {
		h.dumpLogs()
		h.t.Fatalf("intent = %s, want %s", got, want)
	}
}

func (h *harness) wantLease(want bool) {
	h.t.Helper()
	if got := h.m.doc.Lease != nil; got != want {
		h.dumpLogs()
		h.t.Fatalf("lease present = %v, want %v", got, want)
	}
}

// wantLive asserts how many leases the provider still has open, which is the only
// assertion that catches a lease we forgot rather than closed.
func (h *harness) wantLive(want int) {
	h.t.Helper()
	got, err := h.dry.Adopt(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	if len(got) != want {
		h.dumpLogs()
		h.t.Fatalf("provider has %d open lease(s), want %d", len(got), want)
	}
}

// published reads back what the controller actually wrote to its branch, so a
// test asserts on the transport rather than on the machine's memory.
func (h *harness) published() *state.Controller {
	h.t.Helper()
	if err := h.agent.Fetch(h.t.Context()); err != nil {
		h.t.Fatalf("fetch for read-back: %v", err)
	}
	doc, _, _, err := h.agent.ReadController()
	if err != nil {
		h.t.Fatalf("read published state: %v", err)
	}
	return doc
}
