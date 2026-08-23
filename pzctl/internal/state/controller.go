package state

import (
	"time"
)

// DocVersion is the schema version stamped into every document. A reader that
// finds a higher version refuses to write, rather than silently dropping fields
// it does not know about — that is how a rolling upgrade loses a dseq.
const DocVersion = 1

// Lease identifies the Akash deployment. It is the singleton that invariant I1
// is about: at most one of these may be non-nil at a time, and the FSM is its
// only writer.
type Lease struct {
	DSeq     string `json:"dseq"`
	GSeq     int    `json:"gseq"`
	OSeq     int    `json:"oseq"`
	Provider string `json:"provider"`
	// Location is where the provider says it is, for the dashboard — "Prague, CZ"
	// or similar. Recorded at deploy time because it is a property of the bid we
	// took, and the provider list it came from is not read again afterwards. Empty
	// when the provider publishes no geography, which the page treats as "do not
	// claim to know".
	Location string `json:"location,omitempty"`
	// CreatedAt is when the lease was created, used to compute burn.
	CreatedAt Stamp `json:"created_at"`
}

// Endpoint is where players connect. Ports are recorded explicitly rather than
// assumed, because with a shared endpoint the provider chooses them.
type Endpoint struct {
	IP string `json:"ip"`
	// Host is the provider's own hostname, set instead of IP when the server runs
	// on a shared endpoint with no dedicated IP lease. Exactly one of the two is
	// populated: a dedicated IP is an address we hold, a Host is one we borrow, and
	// the difference decides whether the zone gets an A record or a CNAME.
	Host     string `json:"host,omitempty"`
	GamePort int    `json:"game_port"`
	UDPPort  int    `json:"udp_port"`
	// RCONPort is zero when RCON is disabled.
	RCONPort int `json:"rcon_port,omitempty"`
}

// Ready reports whether the endpoint is complete enough to hand to a player.
// v1 used the sentinel string "pending" in the ip field for this; a sentinel
// that has to be string-compared everywhere eventually gets missed.
func (e Endpoint) Ready() bool { return (e.IP != "" || e.Host != "") && e.GamePort > 0 }

// Addr is what a player types, without the port. Host wins when both are set,
// because a provider that reports a hostname may also report the shared ingress
// IP behind it, and the hostname is the one that survives the provider
// renumbering it.
func (e Endpoint) Addr() string {
	if e.Host != "" {
		return e.Host
	}
	return e.IP
}

// Price is what the lease actually costs, as opposed to the ceiling we were
// willing to bid.
type Price struct {
	// AmountPerBlock is in Denom, which is recorded alongside it because the two
	// are meaningless apart: 34 is half a dollar a day in uact and whatever AKT
	// happens to be worth in uakt.
	AmountPerBlock int    `json:"amount_per_block"`
	Denom          string `json:"denom"`
	// AKTUSD is the rate used for the conversion, and is absent for
	// dollar-pegged denominations where no rate was involved.
	AKTUSD     float64 `json:"akt_usd,omitempty"`
	USDPerHour float64 `json:"usd_per_hour"`
	USDPerDay  float64 `json:"usd_per_day"`
	QuotedAt   Stamp   `json:"quoted_at"`
}

// URLs are the controller's own public addresses, discovered after its lease is
// up. The agent reads these from the state branch, which is what removes v1's
// `CONTROLLER_URL=http://controller:8000` — a value that could never resolve,
// and was special-cased as a sentinel meaning "look it up in git instead".
type URLs struct {
	// Public is the operator-facing base URL, behind Cloudflare when DNS is on.
	Public string `json:"public"`
	// Raw is the provider hostname, used when Cloudflare is down or disabled.
	Raw string `json:"raw"`
	// Webhook is where GitHub should POST.
	Webhook string `json:"webhook"`
}

// Base returns the URL a client should try first.
func (u URLs) Base() string {
	if u.Public != "" {
		return u.Public
	}
	return u.Raw
}

// Direct returns the route that reaches the controller without a proxy in the
// middle, and is the address the agent's own traffic should prefer.
//
// The distinction is not cosmetic. Public goes through Cloudflare, whose free plan
// refuses a request body over 100 MB with a 413 — and a backup upload is exactly
// one large request body. A world big enough to be worth backing up is a world
// whose backup cannot be uploaded through the proxy at all. So bulk traffic takes
// the provider's own host:port, and Public stays what it is for: a stable name for
// people.
//
// Falls back to Public rather than to nothing, because a controller that has not
// discovered its own lease address yet still has to be reachable. That makes the
// fallback the pre-existing behaviour, 100 MB cap included, which is the correct
// direction to fail in.
func (u URLs) Direct() string {
	if u.Raw != "" {
		return u.Raw
	}
	return u.Public
}

// BackupRequest is the controller's standing ask for a backup, published on the
// controller's branch. The agent answers it with a BackupReport carrying the same
// ID, and the request stays in the document until that answer arrives — so the
// ask survives a controller restart and an agent restart alike.
//
// The ID is what makes a request answerable exactly once. v1 had no identity on
// either side: a flag said a backup was wanted, and the next archive to appear
// satisfied whatever was outstanding. A halt could therefore be signed off by a
// periodic backup that had started before the halt did, saving a world that was
// several minutes stale.
type BackupRequest struct {
	ID string `json:"id"`
	// Reason is for the log and the dashboard: periodic, operator, or halt.
	Reason      string `json:"reason"`
	RequestedAt Stamp  `json:"requested_at"`
}

// Age is how long the request has been outstanding, which is what the halt and
// backup timeouts are measured against.
func (r *BackupRequest) Age(now time.Time) time.Duration {
	if r == nil || r.RequestedAt.Zero() {
		return 0
	}
	return now.Sub(r.RequestedAt.Time)
}

// Controller is the controller-owned document, living alone on the controller's
// state branch. Nothing else may write it — not by convention, but because the
// agent's credentials push a different ref and its API has no method that
// touches this type.
type Controller struct {
	Version   int   `json:"version"`
	UpdatedAt Stamp `json:"updated_at"`

	// Intent is what the operator asked for. The agent reads it to decide
	// whether a PZ exit means relaunch or park.
	Intent Intent `json:"intent"`

	// Status is the observed lifecycle position, and Since is when it was
	// entered — which is what makes a timeout ("booting for 40 minutes")
	// expressible without a separate clock.
	Status Status `json:"status"`
	Since  Stamp  `json:"since"`

	Lease    *Lease   `json:"lease"`
	Endpoint Endpoint `json:"endpoint"`
	Price    Price    `json:"price"`
	URLs     URLs     `json:"urls"`

	// RestoreTarget is the backup the next boot must restore, or empty for a
	// fresh world. Validated against the index both when set and when read;
	// v1 let it drift until it named a file that had never existed.
	RestoreTarget string `json:"restore_target"`

	// RestorePinned records that an operator chose RestoreTarget by name, rather
	// than it being the newest backup followed automatically. The distinction is
	// the whole point: under the latest policy a completed backup moves the target,
	// and without this flag it would silently overwrite the archive an operator
	// deliberately picked — which is precisely what they would be doing after a
	// backup captured a broken world.
	RestorePinned bool `json:"restore_pinned,omitempty"`

	// BackupRequest is the outstanding ask for a backup, or nil. At most one may
	// be in flight: the agent has one PZ process and one Saves directory, so a
	// second concurrent request could only produce a torn archive.
	BackupRequest *BackupRequest `json:"backup_request"`

	// StopAt is a scheduled shutdown. Nil means none.
	StopAt *Stamp `json:"stop_at"`

	// ProcessedSHAs is a ring buffer of webhook head SHAs already acted on, so
	// a redelivery or our own push cannot re-run a trigger.
	ProcessedSHAs []string `json:"processed_shas"`

	// LastError is the most recent failure, kept for the dashboard. Cleared on
	// the next successful transition.
	LastError string `json:"last_error,omitempty"`
}

// ProcessedSHACap bounds the dedup ring. GitHub redelivers within minutes and
// our own pushes are seen once, so a few dozen is ample; the cap exists to stop
// the document growing without limit.
const ProcessedSHACap = 64

// NewController returns the document for a system that has never run: offline,
// nothing leased, and stopped — the only safe assumption, since a controller
// that guesses "running" on first boot would deploy on its own.
func NewController(loc *time.Location) *Controller {
	return &Controller{
		Version:       DocVersion,
		UpdatedAt:     Now(loc),
		Intent:        IntentStopped,
		Status:        StatusOffline,
		Since:         Now(loc),
		ProcessedSHAs: []string{},
	}
}

// SetStatus applies a status change if the transition table allows it, stamping
// Since with at. An equal status is a no-op that still refreshes UpdatedAt, so
// callers can be idempotent without checking first.
//
// The instant is supplied rather than read from the clock here, and so it is for
// every mutator below. The controller's timeouts are all differences between a
// stamp in this document and its own notion of now — "no ready banner within
// online_timeout", "the backup request is older than halt_timeout" — so if the
// document stamped itself from time.Now while the machine measured from an
// injected clock, those two would be different clocks and every timeout would be
// untestable. One owner, one clock.
func (c *Controller) SetStatus(to Status, at Stamp) error {
	if !CanTransition(c.Status, to) {
		return &TransitionError{From: c.Status, To: to}
	}
	if c.Status != to {
		c.Status = to
		c.Since = at
		c.LastError = ""
	}
	c.UpdatedAt = at
	return nil
}

// Fail records an error and moves to StatusFailed. It is always legal: any
// status may fail, and refusing the transition would leave the document
// describing a cycle that is no longer running.
func (c *Controller) Fail(err error, at Stamp) {
	if err != nil {
		c.LastError = err.Error()
	}
	c.Status = StatusFailed
	c.Since = at
	c.UpdatedAt = at
}

// MarkProcessed records a webhook head SHA, evicting the oldest past the cap.
// It reports false if the SHA was already present, which is the caller's signal
// to ignore the delivery.
func (c *Controller) MarkProcessed(sha string) bool {
	if sha == "" {
		return false
	}
	if c.WasProcessed(sha) {
		return false
	}
	c.ProcessedSHAs = append(c.ProcessedSHAs, sha)
	if n := len(c.ProcessedSHAs); n > ProcessedSHACap {
		c.ProcessedSHAs = append([]string{}, c.ProcessedSHAs[n-ProcessedSHACap:]...)
	}
	return true
}

// WasProcessed reports whether the SHA is in the ring.
func (c *Controller) WasProcessed(sha string) bool {
	for _, s := range c.ProcessedSHAs {
		if s == sha {
			return true
		}
	}
	return false
}

// StopDue reports whether a scheduled stop has come due as of now.
func (c *Controller) StopDue(now time.Time) bool {
	return c.StopAt != nil && !c.StopAt.Zero() && !now.Before(c.StopAt.Time)
}

// RequestBackup records an ask for a backup and returns it. The ID and the
// instant are both supplied by the caller, so this package needs no clock and no
// randomness at all — which is what keeps document behaviour reproducible in a
// test.
//
// An ask already outstanding is returned unchanged. Overwriting it would orphan
// the agent's in-flight work: the report it eventually pushes would carry the old
// ID, match nothing, and be discarded — leaving a halt waiting for a backup that
// had already been taken.
func (c *Controller) RequestBackup(id, reason string, at Stamp) *BackupRequest {
	if c.BackupRequest != nil {
		return c.BackupRequest
	}
	if id == "" {
		// A request with no ID can never be matched to a report, so it would wait
		// forever. Refusing is better than publishing one.
		return nil
	}
	c.BackupRequest = &BackupRequest{ID: id, Reason: reason, RequestedAt: at}
	c.UpdatedAt = at
	return c.BackupRequest
}

// ClearBackupRequest drops the outstanding ask. Called once its report has been
// consumed, or once it has timed out — in both cases the agent must be free to
// accept a new request.
func (c *Controller) ClearBackupRequest(at Stamp) {
	if c.BackupRequest == nil {
		return
	}
	c.BackupRequest = nil
	c.UpdatedAt = at
}

// BackupAnswer reports how the agent's document answers the outstanding request.
// A report for a different request, or no request at all, answers nothing — the
// distinction the halt sequence depends on.
func (c *Controller) BackupAnswer(a *Agent) (BackupState, bool) {
	if c.BackupRequest == nil || a == nil || a.Backup == nil {
		return "", false
	}
	if a.Backup.RequestID != c.BackupRequest.ID {
		return "", false
	}
	return a.Backup.State, true
}

// Agent is the agent-owned document on the agent's state branch.
type Agent struct {
	Version   int   `json:"version"`
	UpdatedAt Stamp `json:"updated_at"`

	// DSeq names the lease this agent is serving, copied from the controller's
	// own document on every publish. It is what makes a report attributable
	// (invariant I16): the branch is one document that outlives the container
	// that wrote it, so a report left behind by a dead lease is still sitting
	// there when the next world boots. The controller acted on one — a "crashed"
	// from a world closed 90 minutes earlier — and halted a server that had been
	// running for two seconds.
	//
	// Empty means the agent had not read the controller's document when it
	// published, which the controller must treat as no report at all rather than
	// as a report about the lease it happens to hold.
	DSeq string `json:"dseq,omitempty"`

	Phase Phase `json:"phase"`
	Since Stamp `json:"since"`

	// PlayersCount is the live connected count, or PlayersUnknown when the
	// agent has not been able to measure it. It is never a literal zero written
	// by a code path that does not know: v1 hardcoded "players_count": 0 at
	// eleven separate write sites, which made the RCON query that would have
	// filled it in dead code and pinned the dashboard to 0 forever.
	PlayersCount int   `json:"players_count"`
	PlayersAt    Stamp `json:"players_at"`

	// Restarts is how many times PZ has been relaunched under the current
	// lease, bounded by server.crash.max_restarts.
	Restarts int `json:"restarts"`

	// LivenessAt is stamped even when nothing changed, so the controller can
	// tell a quiet server from a wedged one.
	LivenessAt Stamp `json:"liveness_at"`

	// Backup reports the outcome of the most recent backup the controller asked
	// for, keyed by the request ID so a stale report cannot satisfy a new
	// request.
	Backup *BackupReport `json:"backup"`

	LastError string `json:"last_error,omitempty"`
}

// PlayersUnknown is the sentinel for "not measured". It is negative so that no
// arithmetic on it can be mistaken for a real count.
const PlayersUnknown = -1

// BackupState is the lifecycle of one requested backup.
type BackupState string

const (
	BackupRunning BackupState = "running"
	BackupDone    BackupState = "done"
	BackupFailed  BackupState = "failed"
)

// BackupReport is the agent's answer to a backup request.
type BackupReport struct {
	// RequestID echoes the controller's request. A report whose ID does not
	// match the outstanding request is ignored, which is what stops the halt
	// sequence from accepting a backup that started before it did.
	RequestID string      `json:"request_id"`
	State     BackupState `json:"state"`
	Name      string      `json:"name,omitempty"`
	Size      int64       `json:"size,omitempty"`
	SHA256    string      `json:"sha256,omitempty"`
	StartedAt Stamp       `json:"started_at"`
	EndedAt   Stamp       `json:"ended_at,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// NewAgent returns the document for an agent that has just started. Note
// PlayersUnknown rather than 0.
func NewAgent(loc *time.Location) *Agent {
	return &Agent{
		Version:      DocVersion,
		UpdatedAt:    Now(loc),
		Phase:        PhaseStarting,
		Since:        Now(loc),
		PlayersCount: PlayersUnknown,
		LivenessAt:   Now(loc),
	}
}

// SetPhase records a phase change, stamping Since with at. Phases have no
// transition table: the agent is a linear supervisor, and constraining it would
// only add a way for a real event to be dropped.
func (a *Agent) SetPhase(p Phase, at Stamp) {
	if a.Phase != p {
		a.Phase = p
		a.Since = at
	}
	a.Touch(at)
}

// SetPlayers records a measured count. Negative input is normalised to
// PlayersUnknown, so a failed measurement cannot be mistaken for an empty
// server — the distinction the pause-when-empty logic depends on.
func (a *Agent) SetPlayers(n int, at Stamp) {
	if n < 0 {
		a.PlayersCount = PlayersUnknown
		a.PlayersAt = Stamp{}
	} else {
		a.PlayersCount = n
		a.PlayersAt = at
	}
	a.Touch(at)
}

// PlayersKnown reports whether PlayersCount is a real measurement.
func (a *Agent) PlayersKnown() bool { return a.PlayersCount >= 0 }

// Touch refreshes the liveness stamp without changing anything else.
func (a *Agent) Touch(at Stamp) {
	a.UpdatedAt = at
	a.LivenessAt = at
}
