package agent

// The agent's half of invariant I16. The controller ignores any report it cannot
// tie to the lease it holds, so a report that does not name one is a report that
// does nothing — which makes this echo load-bearing rather than decorative.

import (
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

func TestPublishedReportsNameTheLeaseTheyServe(t *testing.T) {
	h := newHarness(t)
	h.doc.Lease = &state.Lease{DSeq: "1787420935239", GSeq: 1, OSeq: 1,
		Provider: "akash1hgulk6aekakqzc0v6wukrd3dy9n90f5gkl4ezk"}
	h.publish("a lease to serve")

	h.start()
	online := h.waitPhase(state.PhaseOnline, 60*time.Second)

	if online.DSeq != "1787420935239" {
		t.Fatalf("published dseq = %q, want the lease from the controller's document — "+
			"an unattributed report is one the controller must ignore", online.DSeq)
	}
}

// A lease that changes under a running agent: the container is new, but the branch
// is not, so the next publish has to carry the new dseq or the controller stops
// believing an agent that is in fact fine.
func TestTheEchoFollowsTheControllersLease(t *testing.T) {
	h := newHarness(t)
	h.doc.Lease = &state.Lease{DSeq: "1000000000001", GSeq: 1, OSeq: 1}
	h.publish("first lease")
	h.start()
	h.waitPhase(state.PhaseOnline, 60*time.Second)

	h.doc.Lease = &state.Lease{DSeq: "1000000000002", GSeq: 1, OSeq: 1}
	h.publish("second lease")

	h.waitFor("the report to name the second lease", 60*time.Second, func() bool {
		return h.agentDoc().DSeq == "1000000000002"
	})
}

// The controller clears the lease while closing. A report published in that window
// is about the lease being closed, so it keeps naming it: clearing the echo would
// make the agent's own "stopping" — the report the halt is waiting for —
// unattributable, and the halt would time out instead of completing.
func TestClearingTheLeaseDoesNotClearTheEcho(t *testing.T) {
	h := newHarness(t)
	h.doc.Lease = &state.Lease{DSeq: "1000000000003", GSeq: 1, OSeq: 1}
	h.publish("a lease to serve")
	h.start()
	h.waitPhase(state.PhaseOnline, 60*time.Second)

	h.doc.Lease = nil
	h.doc.Intent = state.IntentStopped
	h.publish("halt, lease cleared")

	stopped := h.waitPhase(state.PhaseStopped, 60*time.Second)
	if stopped.DSeq != "1000000000003" {
		t.Fatalf("published dseq = %q after the lease was cleared, want it kept", stopped.DSeq)
	}
}
