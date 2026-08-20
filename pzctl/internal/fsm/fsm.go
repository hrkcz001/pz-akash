// Package fsm is the controller's decision loop.
//
// One goroutine owns the controller document and is the only thing that writes
// it. Everything that could cause a decision — a webhook, a poll timer, a
// housekeeping tick, the result of a deploy — arrives as an Event on one channel
// and is handled to completion before the next is read. That single property is
// the structural fix for the halt loop: while a sequence is in flight, a
// duplicate trigger is a no-op, because there is no second goroutine to run it.
//
// Two rules shape everything below.
//
// The machine's forward progress must be recoverable from the published document
// alone. There is no waiting inside a handler and no sequence held on a
// goroutine's stack: each status, plus the fields alongside it, says what is
// being waited for. So a controller that is restarted — or redeployed — in the
// middle of a halt picks the halt up where it was, and the two long operations
// that do run off the loop (deploy, close) report back as events.
//
// Long operations must never block the loop. At most one such job exists at a
// time; it holds a cancellable context, and cancelling it is how a halt overtakes
// a deploy in progress.
package fsm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Kind is an event type.
type Kind string

const (
	// KindPoll means "the operator branch may have changed; go and look". It is
	// what both the webhook and the poll timer produce, and that identity is
	// deliberate: the webhook only makes the reaction faster, so losing every
	// delivery costs latency and nothing else.
	KindPoll Kind = "poll"
	// KindTick is housekeeping: timeouts, schedules, periodic backups.
	KindTick Kind = "tick"
	// KindDeployResult and KindCloseResult carry a finished job's outcome.
	KindDeployResult Kind = "deploy_result"
	KindCloseResult  Kind = "close_result"
)

// Event is the only way anything enters the machine.
type Event struct {
	Kind Kind
	// Source names the origin for the log: webhook, poll, tick, startup.
	Source string
	// SHA is the operator-branch head a delivery named, checked against the
	// document's dedup ring.
	SHA string

	deploy *deployOutcome
	closed *closeOutcome
}

// Poll builds the event a webhook or a timer produces.
func Poll(source, sha string) Event { return Event{Kind: KindPoll, Source: source, SHA: sha} }

// Tick builds a housekeeping event.
func Tick(source string) Event { return Event{Kind: KindTick, Source: source} }

type deployOutcome struct {
	res DeployResult
	err error
}

type closeOutcome struct {
	lease state.Lease
	err   error
}

// Deps are the machine's collaborators. Everything is required except the
// injection points, which default to the obvious thing.
type Deps struct {
	Cfg   *config.Config
	Bus   *gitbus.ControllerBus
	Akash Akash

	// ControllerURL is the base URL handed to the agent at deploy time. Empty is
	// legal: the agent then discovers it from the controller's state branch,
	// which is the path invariant I15 exists to guarantee.
	ControllerURL string

	// Now and NewID are injection points for tests. NewID must return a value
	// unique within the lifetime of a backup request.
	Now   func() time.Time
	NewID func() string

	Logf func(format string, args ...any)
}

// Machine is the controller's state machine. Construct with New and drive with
// Run; every other method is either an event source or a read for the dashboard.
type Machine struct {
	cfg   *config.Config
	bus   *gitbus.ControllerBus
	akash Akash
	br    gitbus.Branches
	loc   *time.Location

	ctlURL string
	now    func() time.Time
	newID  func() string
	logf   func(string, ...any)

	events chan Event

	// Owned by the loop goroutine. Nothing else may touch these.
	doc   *state.Controller
	idx   *state.Backups
	agent *state.Agent

	job *job

	// pending is the reason for a publish that the coalescing window has
	// deferred; empty means the document is published.
	pending  string
	lastPush time.Time

	// closeAttempts bounds retries of a failing close, so a provider that always
	// errors does not turn into an infinite push loop.
	closeAttempts int

	// warned remembers one-shot log lines, so an unknown trigger file left in
	// place does not print on every poll.
	warned map[string]bool

	// mu guards only the snapshot readers below, which exist for the dashboard.
	mu   sync.Mutex
	snap Snapshot
}

// eventBuffer is generous relative to the event rate — a handful per hour — so a
// drop means something is genuinely wedged rather than merely busy.
const eventBuffer = 64

// New validates deps and returns a machine positioned at whatever the repository
// last said. It performs no network I/O; Run does that.
func New(d Deps) (*Machine, error) {
	switch {
	case d.Cfg == nil:
		return nil, errors.New("fsm: Cfg is required")
	case d.Bus == nil:
		return nil, errors.New("fsm: Bus is required")
	case d.Akash == nil:
		return nil, errors.New("fsm: Akash is required")
	}
	m := &Machine{
		cfg:    d.Cfg,
		bus:    d.Bus,
		akash:  d.Akash,
		br:     d.Bus.Branches(),
		loc:    d.Cfg.Location(),
		ctlURL: d.ControllerURL,
		now:    d.Now,
		newID:  d.NewID,
		logf:   d.Logf,
		events: make(chan Event, eventBuffer),
		warned: map[string]bool{},
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	if m.newID == nil {
		m.newID = m.timeID
	}
	m.doc = state.NewController(m.loc)
	m.idx = state.NewBackups()
	m.agent = state.NewAgent(m.loc)
	return m, nil
}

// timeID is the default request ID: the request time to the microsecond, which is
// unique for one machine's purposes and, unlike a random string, tells you when
// the request was made when it turns up in a log two days later.
func (m *Machine) timeID() string {
	return strconv.FormatInt(m.now().UnixMicro(), 36)
}

// stamp is the machine's clock rendered in identity.timezone, and is the only
// instant written into either document.
//
// Every timestamp comes from here rather than from the state package's own call to
// time.Now, for two reasons. The document and the machine measure the same
// timeouts against each other — "no ready banner within online_timeout" is
// now minus doc.Since — so two clocks would mean two answers. And the location is
// the configured one, never the host's, which is what makes a state branch diff
// readable to the operator whose timezone it is.
func (m *Machine) stamp() state.Stamp {
	return state.At(m.now()).In(m.loc)
}

// Send offers an event to the loop and reports whether it was accepted.
//
// It never blocks. A full buffer is answered by dropping the event, which is safe
// precisely because KindPoll and KindTick are idempotent: the next one does the
// same work. Job results do not come through here — they use a blocking send from
// the job goroutine, because losing one would leave a status waiting forever.
func (m *Machine) Send(ev Event) bool {
	select {
	case m.events <- ev:
		return true
	default:
		m.logf("fsm: event queue full, dropped %s from %s", ev.Kind, ev.Source)
		return false
	}
}

// Run drives the machine until ctx is cancelled. It returns nil on a clean
// shutdown; the only errors it returns are from the initial load, because a
// controller that cannot read its own state must not start guessing.
func (m *Machine) Run(ctx context.Context) error {
	if err := m.load(ctx); err != nil {
		return err
	}

	tick := time.NewTicker(m.cfg.Controller.Poll.Tick.D())
	defer tick.Stop()
	poll := time.NewTicker(m.pollInterval())
	defer poll.Stop()
	pollEvery := m.pollInterval()

	m.handle(ctx, Poll("startup", ""))

	for {
		// A deferred publish is armed as a one-shot timer rather than left to the
		// next tick, so min_push_interval is a coalescing window and not an extra
		// tick of latency on a halt.
		var flush <-chan time.Time
		var timer *time.Timer
		if m.pending != "" {
			timer = time.NewTimer(m.pushWait())
			flush = timer.C
		}

		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			m.shutdown()
			return nil
		case ev := <-m.events:
			m.handle(ctx, ev)
		case <-tick.C:
			m.handle(ctx, Tick("tick"))
		case <-poll.C:
			m.handle(ctx, Poll("poll", ""))
		case <-flush:
			m.flush(ctx)
		}
		if timer != nil {
			// Stopped explicitly rather than deferred: a defer inside the loop
			// would retain one timer per iteration for the life of the process.
			timer.Stop()
		}

		// The reconcile interval follows the status: an idle system checks git
		// rarely, a running one often enough to notice the agent going online.
		if want := m.pollInterval(); want != pollEvery {
			poll.Reset(want)
			pollEvery = want
			m.logf("fsm: reconcile interval now %s (status %s)", want, m.doc.Status)
		}
	}
}

// Once performs a single reconcile pass: load, read both documents, consume any
// triggers, advance, and wait for whatever job that started to report back.
//
// It exists for CI and for `pzctl controller --once`, where a process that ran
// forever would be useless. Waiting for the job is what makes it meaningful: a
// pass that returned the instant it had launched a deploy would report success
// for a deployment that had not happened yet.
func (m *Machine) Once(ctx context.Context) error {
	if err := m.load(ctx); err != nil {
		return err
	}
	m.handle(ctx, Poll("once", ""))
	for m.job != nil {
		select {
		case <-ctx.Done():
			m.shutdown()
			return ctx.Err()
		case ev := <-m.events:
			m.handle(ctx, ev)
		}
	}
	// The coalescing window may still be open, so publish unconditionally rather
	// than leave the pass's conclusions unrecorded.
	m.lastPush = time.Time{}
	m.flush(ctx)
	m.logf("fsm: one pass done: status=%s intent=%s lease=%s",
		m.doc.Status, m.doc.Intent, leaseName(m.doc.Lease))
	return nil
}

func (m *Machine) pollInterval() time.Duration {
	if m.doc != nil && m.doc.Status != state.StatusOffline {
		return m.cfg.Controller.Poll.Active.D()
	}
	return m.cfg.Controller.Poll.Idle.D()
}

// pushWait is how long the coalescing window still has to run.
func (m *Machine) pushWait() time.Duration {
	wait := m.cfg.Git.MinPushInterval.D() - m.now().Sub(m.lastPush)
	if wait < 0 {
		return 0
	}
	return wait
}

// handle dispatches one event and then publishes anything it changed.
func (m *Machine) handle(ctx context.Context, ev Event) {
	switch ev.Kind {
	case KindPoll:
		m.onPoll(ctx, ev)
	case KindTick:
		m.advance(ctx)
	case KindDeployResult:
		m.onDeployResult(ctx, ev.deploy)
	case KindCloseResult:
		m.onCloseResult(ctx, ev.closed)
	default:
		m.logf("fsm: ignoring unknown event %q from %s", ev.Kind, ev.Source)
	}
	m.snapshot()
	m.flush(ctx)
}

// onPoll is the git-facing half: fetch, read what the other side says, take any
// operator requests, then let the state machine move.
func (m *Machine) onPoll(ctx context.Context, ev Event) {
	if ev.SHA != "" && m.doc.WasProcessed(ev.SHA) {
		// The durable half of the self-push filter. gitbus.WasOurs catches our own
		// echo within one process lifetime; this catches it across a restart, and
		// catches a GitHub redelivery of something we already acted on.
		m.logf("fsm: %s delivery %s already processed", ev.Source, short(ev.SHA))
		return
	}
	if err := m.bus.Fetch(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		// Not fatal. git being unreachable is a transient condition, and a
		// controller that exits on one leaves a lease running with nobody
		// watching it — which is the failure this whole design is built around.
		m.logf("fsm: fetch failed: %v", err)
		return
	}
	m.readAgent()
	m.consumeTriggers(ctx)
	m.advance(ctx)
}

// readAgent refreshes the agent's document. A branch that does not exist yet is
// not an error: it means the agent has never reported.
func (m *Machine) readAgent() {
	doc, rep, err := m.bus.ReadAgent()
	if err != nil {
		m.logf("fsm: read agent state: %v", err)
		return
	}
	if !rep.OK() {
		m.logf("fsm: agent state needed repair: %s", rep)
	}
	if doc.Phase != m.agent.Phase {
		m.logf("fsm: agent phase %s -> %s", m.agent.Phase, doc.Phase)
	}
	m.agent = doc
}

// load establishes the starting position, and is the one place allowed to fail
// hard. Guessing here means guessing about a lease.
func (m *Machine) load(ctx context.Context) error {
	if err := m.bus.Fetch(ctx); err != nil {
		return fmt.Errorf("fsm: initial fetch: %w", err)
	}
	doc, idx, rep, err := m.bus.ReadOwn()
	if err != nil {
		return fmt.Errorf("fsm: read controller state: %w", err)
	}
	m.doc, m.idx = doc, idx
	if !rep.OK() {
		m.logf("fsm: controller state needed repair: %s", rep)
	}
	if rep.Fatal() {
		// The document is defaults, which claim no lease. That claim is exactly
		// the one that costs money if it is wrong, so ask the provider instead of
		// believing it.
		m.adopt(ctx)
	}
	// Publishing is otherwise driven by change, and a cold start changes nothing:
	// offline/stopped is what the zero document already says. That would leave the
	// state branch missing entirely until the first trigger, so anything reading it —
	// the dashboard, an operator, `pzctl status` — would have to treat "no branch"
	// and "no controller" as the same thing. One commit at first sight makes the
	// branch's existence something the rest of the system can rely on.
	if ok, err := m.bus.Exists(m.br.Controller, gitbus.FileController); err == nil && !ok {
		m.dirty("first sight: publishing the initial document")
	}
	m.readAgent()
	m.reconcileLease(ctx)
	m.logf("fsm: start at status=%s intent=%s lease=%s agent=%s",
		m.doc.Status, m.doc.Intent, leaseName(m.doc.Lease), m.agent.Phase)
	m.snapshot()
	return nil
}

// adopt recovers a lease the document lost. Invariant I1 says exactly one
// deployment exists; when the document cannot tell us which, the provider can.
func (m *Machine) adopt(ctx context.Context) {
	leases, err := m.akash.Adopt(ctx)
	if err != nil {
		m.logf("fsm: WARNING unreadable state and Adopt failed (%v) — a lease may be billing unwatched", err)
		return
	}
	if len(leases) == 0 {
		return
	}
	l := leases[0]
	m.doc.Lease = &l
	m.doc.Fail(fmt.Errorf("adopted dseq %s after an unreadable state document; %d lease(s) found",
		l.DSeq, len(leases)), m.stamp())
	m.dirty("adopt dseq " + l.DSeq)
	m.logf("fsm: WARNING adopted dseq %s from the provider; status is failed until you decide", l.DSeq)
}

// reconcileLease checks the document's lease against the provider at startup.
// A lease recorded in a billing status but gone provider-side means the world
// died without us; the opposite means we very much need to know it is there.
func (m *Machine) reconcileLease(ctx context.Context) {
	if m.doc.Lease == nil {
		if m.doc.Status.Billing() && m.doc.Status != state.StatusFailed {
			// A billing status with no lease cannot be acted on: there is nothing
			// to close and nothing to wait for.
			m.doc.Fail(fmt.Errorf("status %s with no lease recorded", m.doc.Status), m.stamp())
			m.dirty("status " + string(m.doc.Status) + " without a lease")
		}
		return
	}
	alive, err := m.akash.Alive(ctx, *m.doc.Lease)
	if err != nil {
		m.logf("fsm: could not verify dseq %s: %v — assuming it is alive", m.doc.Lease.DSeq, err)
		return
	}
	if alive {
		return
	}
	dseq := m.doc.Lease.DSeq
	m.doc.Lease = nil
	m.doc.Endpoint = state.Endpoint{}
	m.doc.ClearBackupRequest(m.stamp())
	m.doc.Fail(fmt.Errorf("dseq %s no longer exists provider-side", dseq), m.stamp())
	m.dirty("dseq " + dseq + " vanished")
	m.logf("fsm: dseq %s is gone; status failed until a start trigger clears it", dseq)
}

// --- publishing ---

// dirty records that the document needs publishing, and why. Handlers call this
// instead of pushing, so a sequence that changes several fields costs one commit.
func (m *Machine) dirty(reason string) {
	if m.pending == "" {
		m.pending = reason
		return
	}
	if m.pending != reason {
		m.pending += "; " + reason
	}
}

// flush publishes the document if it is dirty and the coalescing window has
// passed. Failure leaves it dirty, so the next flush retries.
func (m *Machine) flush(ctx context.Context) {
	if m.pending == "" || ctx.Err() != nil {
		return
	}
	if m.pushWait() > 0 {
		return
	}
	reason := m.pending

	// Fold this process's pushes into the durable dedup ring before writing, so
	// the commits that consumed triggers cannot be replayed after a restart.
	for _, sha := range m.bus.Pushed() {
		m.doc.MarkProcessed(sha)
	}

	sha, err := m.bus.Publish(ctx, m.doc, m.idx, reason)
	if err != nil {
		m.logf("fsm: publish failed (%s): %v", reason, err)
		return
	}
	m.pending = ""
	m.lastPush = m.now()
	if sha != "" {
		m.logf("fsm: published %s — %s", short(sha), reason)
	}
}

// shutdown makes a last attempt to publish, on a context of its own so a
// cancelled Run still records where it stopped. Nothing is torn down beyond
// that: an in-flight job's context is a child of Run's and is already cancelled,
// and the lease it may have created is recorded in the document rather than
// closed here — closing on shutdown is how a redeploy of the controller would
// take the server down with it.
func (m *Machine) shutdown() {
	if m.job != nil {
		m.logf("fsm: shutting down with %s in flight", m.job.what)
	}
	if m.pending == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m.lastPush = time.Time{}
	m.flush(ctx)
}

// --- readers ---

// Snapshot is a copy of what the machine currently believes, for the dashboard
// and for tests. It is a copy because the loop owns the documents.
type Snapshot struct {
	Status  state.Status
	Intent  state.Intent
	Phase   state.Phase
	Lease   *state.Lease
	Backups []state.Backup
	Job     string
	At      time.Time
}

func (m *Machine) snapshot() {
	s := Snapshot{
		Status: m.doc.Status, Intent: m.doc.Intent, Phase: m.agent.Phase,
		At: m.now(),
	}
	if m.doc.Lease != nil {
		l := *m.doc.Lease
		s.Lease = &l
	}
	s.Backups = append([]state.Backup(nil), m.idx.Items...)
	if m.job != nil {
		s.Job = m.job.what
	}
	m.mu.Lock()
	m.snap = s
	m.mu.Unlock()
}

// State returns the latest snapshot. Safe from any goroutine.
func (m *Machine) State() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func leaseName(l *state.Lease) string {
	if l == nil {
		return "none"
	}
	return l.DSeq
}
