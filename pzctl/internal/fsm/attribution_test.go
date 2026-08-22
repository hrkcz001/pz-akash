package fsm

// Invariant I16: an agent report belongs to exactly one lease.
//
// The agent branch is a single document that outlives the container which wrote
// it. Every decision in advance reads it as "what the agent of the lease we hold
// says", and until this file that reading was never enforced: a "crashed" left
// behind by a world closed at 17:59 was still on the branch at 19:52, when a fresh
// server became routable. The controller read it two seconds after the endpoint
// came up, halted the world, and then stopped retrying — correctly, because a halt
// is not a deploy failure. Both live worlds died this way, and the state branch
// recorded none of it: the phase it acted on was true when it was written.
//
// The tests below are about the controller's decisions rather than the field:
// whether a document can steer a lease it does not name.

import (
	"testing"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// staleCrash puts the exact document the dead world left behind on the branch,
// before the controller has any lease at all — which is why it names none.
func staleCrash(h *harness) {
	h.t.Helper()
	h.adoc.SetPhase(state.PhaseCrashed, h.stamp())
	h.adoc.LastError = "no controller URL: PZ_CONTROLLER_URL is unset and the controller has not published its URLs yet"
	h.publishAgentAs("", "agent: crashed: boot failed")
}

func TestAStaleCrashDoesNotHaltAFreshWorld(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	staleCrash(h)

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.wantStatus(state.StatusBooting)

	// The pass that killed the live world. The agent of this lease has published
	// nothing yet, so there is nothing here to act on.
	h.poll()
	h.wantStatus(state.StatusBooting)
	h.wantIntent(state.IntentRunning)
	h.wantLease(true)
	h.wantLive(1)

	if !h.logged("ignoring an agent report") {
		h.dumpLogs()
		t.Fatal("the stale report was neither acted on nor mentioned; an operator has no way to know it is there")
	}
	// The reason still reaches the log even though it is not acted on: it is the
	// only clue that an agent could not reach the controller.
	if !h.logged("PZ_CONTROLLER_URL is unset") {
		h.dumpLogs()
		t.Fatal("the ignored report's last_error was dropped, so the boot failure it describes is invisible")
	}
}

func TestAReportFromThePreviousLeaseIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.wantStatus(state.StatusBooting)

	// An agent that did read a controller document, but the one from the lease
	// before this — the attempt-1 dseq of the live failure.
	h.adoc.SetPhase(state.PhaseCrashed, h.stamp())
	h.publishAgentAs("1787420278924", "agent: crashed on the previous lease")

	h.poll()
	h.wantStatus(state.StatusBooting)
	h.wantIntent(state.IntentRunning)
	h.wantLive(1)
}

// The other half of the invariant: attribution must not make the controller deaf.
// A report that does name our lease is acted on exactly as before.
func TestAnAttributedCrashStillHalts(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.wantStatus(state.StatusBooting)

	h.agentPhase(state.PhaseCrashed) // attributed to the lease we hold
	h.poll()
	h.wantStatus(state.StatusStopping)
	h.wantIntent(state.IntentStopped)
	if !h.logged("agent parked during boot") {
		h.dumpLogs()
		t.Fatal("a crash from this lease's own agent was ignored")
	}
}

func TestAnUnattributedOnlineIsNotBelieved(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	// An "online" from a world that is gone is the mirror image of the crash, and
	// the more expensive mistake: the controller would publish a game endpoint and
	// tell players to connect to a server that is still unpacking mods.
	h.adoc.SetPhase(state.PhaseOnline, h.stamp())
	h.publishAgentAs("", "agent: online in a previous world")

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.poll()
	h.wantStatus(state.StatusBooting)
}

// Recovery, which is what makes ignoring safe: the real agent's first attributed
// report is acted on, so the gate delays nothing beyond one poll.
func TestTheRealAgentTakesOverAfterAStaleReport(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	staleCrash(h)

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.poll()
	h.wantStatus(state.StatusBooting)

	h.agentPhase(state.PhaseOnline)
	h.poll()
	h.wantStatus(state.StatusOnline)
}

// A mismatch persists until the new agent publishes — or forever, if it never can.
// One line per pass would bury the boot it is describing.
func TestAnUnattributableReportIsLoggedOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	staleCrash(h)

	h.trigger("start", "")
	h.poll()
	h.settle()
	h.poll()
	h.poll()
	h.tick()

	if n := h.logCount("ignoring an agent report"); n != 1 {
		h.dumpLogs()
		t.Fatalf("logged the same ignored report %d times, want 1", n)
	}
}
