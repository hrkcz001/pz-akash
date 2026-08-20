// Package state defines the documents the controller and the agent exchange,
// and the rules for reading and writing them safely.
//
// The v1 system built these documents with shell string interpolation:
//
//	echo "{\"ip\": \"$cur_ip\", ..., \"price_per_hour\": $p_hr}" > server_info.json
//
// An empty $p_hr produced `"price_per_hour": ,` — a file no JSON parser accepts,
// which then emptied every field the controller read from it, which in turn
// disabled periodic backups and restore handling. Three rules here make that
// impossible rather than unlikely:
//
//  1. Documents are only ever produced by marshalling a Go struct. There is no
//     code path that assembles JSON from strings.
//  2. Writes are atomic: temp file, fsync, rename. A reader never sees a
//     half-written document.
//  3. Reads never fail hard. A malformed field is reset to its zero value and
//     reported as a Repair; a malformed document yields defaults and a fatal
//     Repair. Callers log loudly and keep running, because the alternative —
//     a controller that dies on a bad byte — is what left a lease billing with
//     nobody watching it.
package state

import "fmt"

// Status is the server lifecycle as observed by the controller. It is the
// controller's view of the world; the agent reports Phase instead, and the
// controller reconciles the two.
type Status string

const (
	// StatusOffline means no Akash deployment exists. This is the only status
	// in which no money is being spent.
	StatusOffline Status = "offline"
	// StatusDeploying covers bid collection and lease creation.
	StatusDeploying Status = "deploying"
	// StatusBooting means the lease is up and the agent is preparing the world.
	StatusBooting Status = "booting"
	// StatusOnline means the agent reported PZ accepting connections.
	StatusOnline Status = "online"
	// StatusBackingUp is a backup in flight while the server stays up.
	StatusBackingUp Status = "backing_up"
	// StatusStopping is a graceful shutdown: final backup, then PZ exit.
	StatusStopping Status = "stopping"
	// StatusClosing is the Akash deployment being closed.
	StatusClosing Status = "closing"
	// StatusFailed means the cycle aborted and an operator should look. The
	// lease may or may not exist, so reconciliation against Akash is required
	// before anything else happens.
	StatusFailed Status = "failed"
)

// Statuses lists every valid status in lifecycle order.
func Statuses() []Status {
	return []Status{
		StatusOffline, StatusDeploying, StatusBooting, StatusOnline,
		StatusBackingUp, StatusStopping, StatusClosing, StatusFailed,
	}
}

// Valid reports whether s is a status this version knows.
func (s Status) Valid() bool {
	for _, k := range Statuses() {
		if s == k {
			return true
		}
	}
	return false
}

// Busy reports whether a multi-step operation is in flight. The FSM drops
// duplicate triggers while busy, which is the structural fix for the halt loop:
// v1 had no such guard, so every webhook that arrived during a halt started
// another parallel halt.
func (s Status) Busy() bool {
	switch s {
	case StatusDeploying, StatusBooting, StatusBackingUp, StatusStopping, StatusClosing:
		return true
	}
	return false
}

// Billing reports whether a lease may exist in this status, and therefore
// whether money may be draining. Used by the reconciler to decide when a
// missing dseq is worth shouting about.
func (s Status) Billing() bool {
	switch s {
	case StatusBooting, StatusOnline, StatusBackingUp, StatusStopping, StatusClosing, StatusFailed:
		return true
	}
	return false
}

// transitions is the complete set of legal status changes. Anything absent is
// rejected and logged rather than applied, so a confused caller cannot walk the
// state backwards into a shape the rest of the system does not expect.
var transitions = map[Status][]Status{
	StatusOffline:   {StatusDeploying, StatusFailed},
	StatusDeploying: {StatusBooting, StatusOffline, StatusClosing, StatusFailed},
	StatusBooting:   {StatusOnline, StatusStopping, StatusClosing, StatusFailed},
	StatusOnline:    {StatusBackingUp, StatusStopping, StatusClosing, StatusFailed},
	StatusBackingUp: {StatusOnline, StatusStopping, StatusClosing, StatusFailed},
	StatusStopping:  {StatusClosing, StatusOffline, StatusFailed},
	StatusClosing:   {StatusOffline, StatusFailed},
	// Failed is recoverable only by going through Offline, which forces the
	// operator (or the reconciler) to establish that no lease is left behind.
	StatusFailed: {StatusOffline},
}

// CanTransition reports whether from -> to is legal. A transition to the same
// status is not a change and is reported as legal so idempotent callers need no
// special case.
func CanTransition(from, to Status) bool {
	if from == to {
		return from.Valid()
	}
	if !to.Valid() {
		return false
	}
	for _, ok := range transitions[from] {
		if ok == to {
			return true
		}
	}
	return false
}

// Intent is what the operator wants, as opposed to what is currently true. The
// agent reads it to decide whether a PZ exit means "relaunch" or "park".
//
// v1 wrote the equivalent file (desired_state) and then never read it from the
// container at all, which is why a crashed server would come back during a halt.
type Intent string

const (
	IntentRunning Intent = "running"
	IntentStopped Intent = "stopped"
)

func (i Intent) Valid() bool { return i == IntentRunning || i == IntentStopped }

// Phase is the agent's report of what the container is actually doing. It is
// written only by the agent, on its own branch.
type Phase string

const (
	// PhaseStarting is the agent alive but PZ not yet launched.
	PhaseStarting Phase = "starting"
	// PhaseRestoring is a backup being downloaded and unpacked.
	PhaseRestoring Phase = "restoring"
	// PhaseRestoreFailed means a requested restore could not be applied. The
	// agent parks here instead of booting a fresh world over the request.
	PhaseRestoreFailed Phase = "restore_failed"
	// PhaseOnline means PZ printed its ready banner.
	PhaseOnline Phase = "online"
	// PhaseSaving is a save/zip/upload cycle in progress.
	PhaseSaving Phase = "saving"
	// PhaseStopping is PZ being asked to quit.
	PhaseStopping Phase = "stopping"
	// PhaseStopped means PZ exited and the agent is parked. The container
	// process stays alive on purpose: see Parked.
	PhaseStopped Phase = "stopped"
	// PhaseCrashed means PZ exited unexpectedly and the restart budget is
	// exhausted. Also parked.
	PhaseCrashed Phase = "crashed"
)

// Phases lists every valid phase.
func Phases() []Phase {
	return []Phase{
		PhaseStarting, PhaseRestoring, PhaseRestoreFailed, PhaseOnline,
		PhaseSaving, PhaseStopping, PhaseStopped, PhaseCrashed,
	}
}

func (p Phase) Valid() bool {
	for _, k := range Phases() {
		if p == k {
			return true
		}
	}
	return false
}

// Parked reports whether the agent has stopped doing work and is now merely
// holding PID 1 open.
//
// This is the fix for the restart loop. In v1 the entrypoint ran `exit
// $EXIT_CODE` when PZ finished; Kubernetes restartPolicy is Always and Akash
// SDL v2.0 gives no way to change it, so the kubelet restarted the container,
// which re-armed a restore and re-announced itself as booting — in the middle of
// a halt. The v2 agent never exits. Closing the lease is what removes the pod,
// and that decision belongs to the controller alone.
func (p Phase) Parked() bool {
	switch p {
	case PhaseStopped, PhaseCrashed, PhaseRestoreFailed:
		return true
	}
	return false
}

// ImpliedStatus maps an agent phase to the status the controller should observe
// when it sees that phase, or "" when the phase implies nothing (because the
// controller's own operation is authoritative at that moment).
func (p Phase) ImpliedStatus() Status {
	switch p {
	case PhaseStarting, PhaseRestoring:
		return StatusBooting
	case PhaseOnline:
		return StatusOnline
	case PhaseSaving:
		return StatusBackingUp
	case PhaseStopping:
		return StatusStopping
	}
	// Stopped, Crashed and RestoreFailed deliberately imply nothing: whether
	// they mean "close the lease" or "this was expected" depends on Intent,
	// which only the controller knows.
	return ""
}

// TransitionError reports a rejected status change.
type TransitionError struct {
	From, To Status
}

func (e *TransitionError) Error() string {
	if !e.To.Valid() {
		return fmt.Sprintf("unknown status %q", e.To)
	}
	return fmt.Sprintf("illegal transition %s -> %s", e.From, e.To)
}
